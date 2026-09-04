// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

// The golink server runs http://go/, a private shortlink service for tailnets.
package golink

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/xsrftoken"
	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/envknob"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/util/dnsname"
)

const (
	defaultHostname = "go"

	// Used as a placeholder short name for generating the XSRF defense token,
	// when creating new links.
	newShortName = ".new"

	// If the caller sends this header set to a non-empty value, we will allow
	// them to make the call even without an XSRF token. JavaScript in browser
	// cannot set this header, per the [Fetch Spec].
	//
	// [Fetch Spec]: https://fetch.spec.whatwg.org
	secHeaderName = "Sec-Golink"
)

var (
	verbose           = flag.Bool("verbose", false, "be verbose")
	controlURL        = flag.String("control-url", ipn.DefaultControlURL, "the URL base of the control plane (i.e. coordination server)")
	sqlitefile        = flag.String("sqlitedb", "", "path of SQLite database to store links")
	dev               = flag.String("dev-listen", "", "if non-empty, listen on this addr and run in dev mode; auto-set sqlitedb if empty and don't use tsnet")
	useHTTPS          = flag.Bool("https", true, "serve golink over HTTPS if enabled on tailnet")
	snapshot          = flag.String("snapshot", "", "file path of snapshot file")
	hostname          = flag.String("hostname", defaultHostname, "service name")
	configDir         = flag.String("config-dir", "", `tsnet configuration directory ("" to use default)`)
	resolveFromBackup = flag.String("resolve-from-backup", "", "resolve a link from snapshot file and exit")
	allowUnknownUsers = flag.Bool("allow-unknown-users", false, "allow unknown users to save links")
	readonly          = flag.Bool("readonly", false, "start golink server in read-only mode")
	openLinks         = flag.Bool("open-links", false, "allow any user to edit any link that its owner has not locked")
	ownerCanLock      = flag.Bool("owner-can-lock", false, "let the owner of a link lock it, as well as an admin; only meaningful with -open-links")
	authEmailHeader   = flag.String("auth-email-header", "", `if non-empty, identify users by this HTTP header, set by an authenticating proxy in front of golink (e.g. "X-Auth-Request-Email"), rather than by their tailnet identity`)
	authGroupsHeader  = flag.String("auth-groups-header", "", `HTTP header holding the comma-separated groups a user belongs to (e.g. "X-Auth-Request-Groups"); only read when -auth-email-header is set`)
	advertiseTags     = flag.String("advertise-tags", os.Getenv("TS_ADVERTISE_TAGS"), "comma-separated list of ACL tags to advertise (e.g. tag:golink)")
	serviceName       = flag.String("register-as-service", envknob.String("TS_SERVICE_NAME"), "register as a Tailscale Service (e.g., svc:golink); requires tagged node")
)

var stats struct {
	mu     sync.Mutex
	clicks ClickStats // short link -> number of times visited

	// dirty identifies short link clicks that have not yet been stored.
	dirty ClickStats
}

var (
	clickCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "golink_clicks_total",
			Help: "Total number of clicks for a recognized GoLink",
		},
		[]string{"path"},
	)
	clickNotFound = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "golink_not_found_total",
			Help: "Total number of clicks for a GoLink doesn't exist",
		},
		[]string{"path"},
	)
	totalLinkCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "golinks_total",
			Help: "Total number of GoLinks being served",
		},
	)
)

// LastSnapshot is the data snapshot (as returned by the /.export handler)
// that will be loaded on startup.
var LastSnapshot []byte

//go:embed static tmpl/*.html tmpl/*.xml
var embeddedFS embed.FS

// db stores short links.
var db *SQLiteDB

var localClient *local.Client

func Run() error {
	flag.Parse()

	if *authEmailHeader != "" {
		// Identity comes from a proxy in front of golink, not from the tailnet.
		currentUser = proxyUser
	}

	hostinfo.SetApp("golink")

	// if resolving from backup, set sqlitefile and snapshot flags to
	// restore links into an in-memory sqlite database.
	if *resolveFromBackup != "" {
		*sqlitefile = ":memory:"
		snapshot = resolveFromBackup
		if flag.NArg() != 1 {
			log.Fatal("--resolve-from-backup also requires a link to be resolved")
		}
	}

	if *sqlitefile == "" {
		if devMode() {
			tmpdir, err := os.MkdirTemp("", "golink_dev_*")
			if err != nil {
				return err
			}
			*sqlitefile = filepath.Join(tmpdir, "golink.db")
			log.Printf("Dev mode temp db: %s", *sqlitefile)
		} else {
			return errors.New("--sqlitedb is required")
		}
	}

	var err error
	if db, err = NewSQLiteDB(*sqlitefile); err != nil {
		return fmt.Errorf("NewSQLiteDB(%q): %w", *sqlitefile, err)
	}

	if *snapshot != "" {
		if LastSnapshot != nil {
			log.Printf("LastSnapshot already set; ignoring --snapshot")
		} else {
			var err error
			LastSnapshot, err = os.ReadFile(*snapshot)
			if err != nil {
				log.Fatalf("error reading snapshot file %q: %v", *snapshot, err)
			}
		}
	}
	if err := restoreLastSnapshot(); err != nil {
		log.Printf("restoring snapshot: %v", err)
	}
	if err := initStats(); err != nil {
		log.Printf("initializing stats: %v", err)
	}
	if err := initMetricsData(); err != nil {
		log.Printf("initializing metrics data: %v", err)
	}

	// if link specified on command line, resolve and exit
	if flag.NArg() > 0 {
		u, err := url.Parse(flag.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		dst, err := resolveLink(u)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(dst.String())
		os.Exit(0)
	}

	// flush stats periodically
	go flushStatsLoop()

	if *dev != "" {
		// override default hostname for dev mode
		if *hostname == defaultHostname {
			if h, p, err := net.SplitHostPort(*dev); err == nil {
				if h == "" {
					h = "localhost"
				}
				*hostname = fmt.Sprintf("%s:%s", h, p)
			}
		}

		log.Printf("Running in dev mode on %s ...", *dev)
		log.Fatal(http.ListenAndServe(*dev, serveHandler()))
	}

	if *hostname == "" {
		return errors.New("--hostname, if specified, cannot be empty")
	}

	tags, err := parseAdvertiseTags(*advertiseTags)
	if err != nil {
		return err
	}

	// create tsNet server and wait for it to be ready & connected.
	srv := &tsnet.Server{
		ControlURL:    *controlURL,
		Dir:           *configDir,
		Hostname:      *hostname,
		Logf:          func(format string, args ...any) {},
		RunWebClient:  true,
		AdvertiseTags: tags,
	}
	if *verbose {
		srv.Logf = log.Printf
	}
	if err := srv.Start(); err != nil {
		return err
	}

	localClient, _ = srv.LocalClient()
out:
	for {
		upCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		status, err := srv.Up(upCtx)
		if err == nil && status != nil {
			break out
		}
	}

	statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := localClient.Status(statusCtx)
	if err != nil {
		return err
	}
	enableTLS := *useHTTPS && status.Self.HasCap(tailcfg.CapabilityHTTPS) && len(srv.CertDomains()) > 0
	fqdn := strings.TrimSuffix(status.Self.DNSName, ".")

	httpHandler := serveHandler()

	// Service registration mode: use ListenService instead of standard listeners
	if *serviceName != "" {
		if !strings.HasPrefix(*serviceName, "svc:") {
			return fmt.Errorf("service name must start with 'svc:' prefix, got: %q", *serviceName)
		}

		log.Printf("Registering as Tailscale Service: %s", *serviceName)
		serviceListener, err := srv.ListenService(*serviceName, tsnet.ServiceModeHTTP{
			HTTPS: true,
			Port:  443,
		})
		if err != nil {
			if errors.Is(err, tsnet.ErrUntaggedServiceHost) {
				return fmt.Errorf("service registration requires a tagged node; add a tag like 'tag:golink' to this node in the Tailscale admin console")
			}
			return fmt.Errorf("failed to register service: %w", err)
		}

		httpsHandler := HSTS(httpHandler)
		log.Printf("Serving https://%s/ as service %s ...", fqdn, *serviceName)
		return http.Serve(serviceListener, httpsHandler)
	}

	// Standard mode: use regular listeners
	if enableTLS {
		httpsHandler := HSTS(httpHandler)
		httpHandler = redirectHandler(fqdn)

		httpsListener, err := srv.ListenTLS("tcp", ":443")
		if err != nil {
			return err
		}
		log.Println("Listening on :443")
		go func() {
			log.Printf("Serving https://%s/ ...", fqdn)
			if err := http.Serve(httpsListener, httpsHandler); err != nil {
				log.Fatal(err)
			}
		}()
	}

	httpListener, err := srv.Listen("tcp", ":80")
	log.Println("Listening on :80")
	if err != nil {
		return err
	}
	log.Printf("Serving http://%s/ ...", *hostname)
	if err := http.Serve(httpListener, httpHandler); err != nil {
		return err
	}

	return nil
}

var (
	// homeTmpl is the template used by the http://go/ index page where you can
	// create or edit links.
	homeTmpl *template.Template

	// detailTmpl is the template used by the link detail page to view or edit links.
	detailTmpl *template.Template

	// successTmpl is the template used when a link is successfully created or updated.
	successTmpl *template.Template

	// helpTmpl is the template used by the http://go/.help page
	helpTmpl *template.Template

	// allTmpl is the template used by the http://go/.all page
	allTmpl *template.Template

	// deleteTmpl is the template used after a link has been deleted.
	deleteTmpl *template.Template

	// opensearchTmpl is the template used by the http://go/.opensearch page
	opensearchTmpl *template.Template

	// searchTmpl is the template used by the http://go/.search page
	searchTmpl *template.Template
)

type visitData struct {
	Short     string
	NumClicks int
}

// searchResult is a link paired with its current click count, used to render
// the listing template (searchTmpl) served by /.all and /.search.
type searchResult struct {
	*Link
	NumClicks int
}

// searchData is the data used by the searchTmpl template.
type searchData struct {
	// Query is the search these results answer, empty when every link is
	// listed.
	Query   string
	Results []searchResult
}

// searchResults annotates links with their current click counts (read from the
// live in-memory counter, the same source the home page uses), preserving the
// historical alphabetical ordering by short name.
func searchResults(links []*Link) []searchResult {
	stats.mu.Lock()
	results := make([]searchResult, len(links))
	for i, link := range links {
		results[i] = searchResult{Link: link, NumClicks: stats.clicks[link.Short]}
	}
	stats.mu.Unlock()

	sort.Slice(results, func(i, j int) bool {
		return results[i].Short < results[j].Short
	})
	return results
}

// homeData is the data used by homeTmpl.
type homeData struct {
	Short    string
	Long     string
	Clicks   []visitData
	XSRF     string
	ReadOnly bool
	User     string
}

// deleteData is the data used by deleteTmpl.
type deleteData struct {
	Short   string
	Long    string
	Pattern string
	XSRF    string
}

var xsrfKey string

func init() {
	homeTmpl = newTemplate("base.html", "home.html")
	detailTmpl = newTemplate("base.html", "detail.html")
	successTmpl = newTemplate("base.html", "success.html")
	helpTmpl = newTemplate("base.html", "help.html")
	deleteTmpl = newTemplate("base.html", "delete.html")
	opensearchTmpl = newTemplate("opensearch.xml")
	searchTmpl = newTemplate("base.html", "search.html")

	b := make([]byte, 24)
	rand.Read(b)
	xsrfKey = base64.StdEncoding.EncodeToString(b)

	initMetrics()
}

var tmplFuncs = template.FuncMap{
	// go is a template function that returns the hostname of the golink service.
	// This is used throughout the UI to render links, but does not impact link resolution.
	"go": func() string {
		if devMode() {
			// in dev mode, just use "go" instead of "localhost:8080"
			return defaultHostname
		}
		return *hostname
	},
}

// newTemplate creates a new template with the specified files in the tmpl directory.
// The first file name is used as the template name,
// and tmplFuncs are registered as available funcs.
// This func panics if unable to parse files.
func newTemplate(files ...string) *template.Template {
	if len(files) == 0 {
		return nil
	}
	tf := make([]string, 0, len(files))
	for _, f := range files {
		tf = append(tf, "tmpl/"+f)
	}
	t := template.New(files[0]).Funcs(tmplFuncs)
	return template.Must(t.ParseFS(embeddedFS, tf...))
}

// initMetrics initializes Prometheus Metrics
func initMetrics() {
	prometheus.MustRegister(clickCounter)
	prometheus.MustRegister(clickNotFound)
	prometheus.MustRegister(totalLinkCount)
}

// initMetricsData set metrics to what is represented in the DB
func initMetricsData() error {
	// Set the totalLinkCount metric to what is saved in the DB
	var count float64
	err := db.db.QueryRow("SELECT COUNT(DISTINCT id) FROM Links").Scan(&count)
	if err != nil {
		return err
	}
	totalLinkCount.Set(count)

	return nil
}

// initStats initializes the in-memory stats counter with counts from db.
func initStats() error {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	clicks, err := db.LoadStats()
	if err != nil {
		return err
	}

	stats.clicks = clicks
	stats.dirty = make(ClickStats)

	return nil
}

// flushStats writes any pending link stats to db.
func flushStats() error {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	if len(stats.dirty) == 0 {
		return nil
	}

	if err := db.SaveStats(stats.dirty); err != nil {
		return err
	}
	stats.dirty = make(ClickStats)
	return nil
}

// flushStatsLoop will flush stats every minute.  This function never returns.
func flushStatsLoop() {
	for {
		if err := flushStats(); err != nil {
			log.Printf("flushing stats: %v", err)
		}
		time.Sleep(time.Minute)
	}
}

// deleteLinkStats removes the link stats from memory.
func deleteLinkStats(link *Link) {
	totalLinkCount.Dec()
	stats.mu.Lock()
	delete(stats.clicks, link.Short)
	delete(stats.dirty, link.Short)
	stats.mu.Unlock()

	db.DeleteStats(link.Short)
}

// redirectHandler returns the http.Handler for serving all plaintext HTTP
// requests. It redirects all requests to the HTTPs version of the same URL.
func redirectHandler(hostname string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := &url.URL{
			Scheme:   "https",
			Host:     hostname,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
}

// HSTS wraps the provided handler and sets Strict-Transport-Security header on
// responses. It inspects the Host header to ensure we do not specify HSTS
// response on non fully qualified domain name origins.
func HSTS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, found := r.Header["Host"]
		if found {
			host := host[0]
			fqdn, err := dnsname.ToFQDN(host)
			if err == nil {
				segCount := fqdn.NumLabels()
				if segCount > 1 {
					w.Header().Set("Strict-Transport-Security", "max-age=31536000")
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// serverHandler returns the main http.Handler for serving all requests.
func serveHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.detail/", serveDetail)
	mux.HandleFunc("/.export", serveExport)
	mux.HandleFunc("/.export-stats", serveExportStats)
	mux.HandleFunc("/.help", serveHelp)
	mux.HandleFunc("/.opensearch", serveOpenSearch)
	mux.HandleFunc("/.all", serveAll)
	mux.HandleFunc("/.delete/", serveDelete)
	mux.HandleFunc("/.search", serveSearch)
	mux.Handle("/.metrics", promhttp.Handler())
	mux.Handle("/.static/", http.StripPrefix("/.", http.FileServer(http.FS(embeddedFS))))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never send a Referer header to link destinations, which would
		// otherwise expose the golink host (and thus the tailnet name) to
		// external sites. Setting the policy on redirect responses also
		// strips any referrer inherited from the page that linked to the
		// go link.
		w.Header().Set("Referrer-Policy", "no-referrer")

		// all internal URLs begin with a leading "."; any other URL is treated as a go link.
		// Serve go links directly without passing through the ServeMux,
		// which sometimes modifies the request URL path, which we don't want.
		if !strings.HasPrefix(r.URL.Path, "/.") {
			serveGo(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func serveHome(w http.ResponseWriter, r *http.Request, short string) {
	var clicks []visitData

	stats.mu.Lock()
	for short, numClicks := range stats.clicks {
		clicks = append(clicks, visitData{
			Short:     short,
			NumClicks: numClicks,
		})
	}
	stats.mu.Unlock()

	sort.Slice(clicks, func(i, j int) bool {
		if clicks[i].NumClicks != clicks[j].NumClicks {
			return clicks[i].NumClicks > clicks[j].NumClicks
		}
		return clicks[i].Short < clicks[j].Short
	})
	if len(clicks) > 200 {
		clicks = clicks[:200]
	}

	var long string
	if short != "" && localClient != nil {
		// if a peer exists with the short name, suggest it as the long URL
		st, err := localClient.Status(r.Context())
		if err == nil {
			for _, p := range st.Peer {
				if host, _, ok := strings.Cut(p.DNSName, "."); ok && host == short {
					long = "http://" + host + "/"
					break
				}
			}
		}
	}

	cu, err := requestUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	homeTmpl.Execute(w, homeData{
		Short:    short,
		Long:     long,
		Clicks:   clicks,
		XSRF:     xsrftoken.Generate(xsrfKey, cu.login, newShortName),
		ReadOnly: *readonly,
		User:     cu.login,
	})
}

func serveAll(w http.ResponseWriter, _ *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := db.LoadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	searchTmpl.Execute(w, searchData{Results: searchResults(links)})
}

func serveHelp(w http.ResponseWriter, _ *http.Request) {
	helpTmpl.Execute(w, nil)
}

func serveOpenSearch(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	opensearchTmpl.Execute(w, nil)
}

// loadLink loads the link with the specified short name, retrying without any
// trailing punctuation. That catches auto-linking and copy/paste issues that
// include punctuation.
func loadLink(short string) (*Link, error) {
	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		if s := strings.TrimRight(short, ".,()[]{}"); s != short {
			link, err = db.Load(s)
		}
	}
	return link, err
}

// lookupLink returns the link that answers to the specified path, along with
// the rest of the path to pass to the link's destination.
//
// Every link answers to its own name, which may itself contain slashes. A link
// with a pattern answers to the paths below its name as well, the longest such
// name winning; a link without one answers to nothing but its own name, so
// that a path below it is free to become a link of its own.
func lookupLink(path string) (*Link, string, error) {
	link, err := loadLink(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return link, "", err
	}
	for i := strings.LastIndex(path, "/"); i > 0; i = strings.LastIndex(path[:i], "/") {
		if l, lerr := loadLink(path[:i]); lerr == nil && l.Pattern != "" {
			return l, path[i+1:], nil
		}
	}
	return nil, "", fs.ErrNotExist
}

func serveGo(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		switch r.Method {
		case "GET":
			serveHome(w, r, "")
		case "POST":
			serveSave(w, r)
		}
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")

	// redirect {name}+ links to /.detail/{name}
	if strings.HasSuffix(path, "+") {
		http.Redirect(w, r, "/.detail/"+strings.TrimSuffix(path, "+"), http.StatusFound)
		return
	}

	link, remainder, err := lookupLink(path)
	if errors.Is(err, fs.ErrNotExist) {
		clickNotFound.WithLabelValues(path).Inc()
		w.WriteHeader(http.StatusNotFound)
		serveHome(w, r, path)
		return
	}
	if err != nil {
		clickNotFound.WithLabelValues(path).Inc()
		log.Printf("serving %q: %v", path, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	clickCounter.WithLabelValues(link.Short).Inc()

	stats.mu.Lock()
	if stats.clicks == nil {
		stats.clicks = make(ClickStats)
	}
	stats.clicks[link.Short]++
	if stats.dirty == nil {
		stats.dirty = make(ClickStats)
	}
	stats.dirty[link.Short]++
	stats.mu.Unlock()

	cu, _ := requestUser(r)
	env := expandEnv{Now: time.Now().UTC(), Path: remainder, user: cu.login, query: r.URL.Query()}
	target, err := resolveTarget(link, env)
	if err != nil {
		log.Printf("expanding %q: %v", link.Long, err)
		if errors.Is(err, errNoUser) {
			http.Error(w, "link requires a valid user", http.StatusUnauthorized)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// http.Redirect always cleans the redirect URL, which we don't always want.
	// Instead, manually set status and Location header.
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
}

// acceptHTML returns whether the request can accept a text/html response.
func acceptHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// detailData is the data used by the detailTmpl template.
type detailData struct {
	// Editable indicates whether the current user can edit the link.
	Editable bool
	// Lockable indicates whether the current user can change whether the link
	// is locked.
	Lockable      bool
	Link          *Link
	XSRF          string
	AlreadyExists bool
}

func serveDetail(w http.ResponseWriter, r *http.Request) {
	short := strings.TrimPrefix(r.URL.Path, "/.detail/")

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if short != link.Short {
		// redirect to canonical short name
		http.Redirect(w, r, "/.detail/"+link.Short, http.StatusFound)
		return
	}
	if err != nil {
		log.Printf("serving detail %q: %v", short, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !acceptHTML(r) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(link)
		return
	}

	cu, err := requestUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	canEdit := canEditLink(r.Context(), link, cu)
	ownerExists, err := userExists(r.Context(), link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}

	data := detailData{
		Link:     link,
		Editable: canEdit,
		Lockable: canEdit && canLockLink(link, cu),
		XSRF:     xsrftoken.Generate(xsrfKey, cu.login, link.Short),
	}
	if r.URL.Query().Get("exists") == "1" {
		data.AlreadyExists = true
	}
	if canEdit && !ownerExists {
		data.Link.Owner = cu.login
	}

	detailTmpl.Execute(w, data)
}

// serveSearch handles requests to /.search?q={query}. A query of
// "owner:<email>" lists the links owned by that user; anything else lists the
// links whose short name, destination or pattern contains it.
func serveSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Redirect(w, r, "/.all", http.StatusFound)
		return
	}

	var links []*Link
	var err error
	if owner, found := strings.CutPrefix(query, "owner:"); found {
		links, err = db.GetLinksByOwner(strings.TrimSpace(owner))
	} else {
		links, err = db.SearchLinks(query)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	searchTmpl.Execute(w, searchData{Query: query, Results: searchResults(links)})
}

type expandEnv struct {
	Now time.Time

	// Path is the remaining path after short name.  For example, in
	// "http://go/who/amelie", Path is "amelie".
	Path string

	// user is the current user, if any.
	// For example, "foo@example.com" or "foo@github".
	user string

	// query is the query parameters from the original request.
	query url.Values
}

var errNoUser = errors.New("no user")

// User returns the current user, or errNoUser if there is no user.
func (e expandEnv) User() (string, error) {
	if e.user == "" {
		return "", errNoUser
	}
	return e.user, nil
}

var expandFuncMap = texttemplate.FuncMap{
	"PathEscape":  url.PathEscape,
	"QueryEscape": url.QueryEscape,
	"TrimPrefix":  strings.TrimPrefix,
	"TrimSuffix":  strings.TrimSuffix,
	"ToLower":     strings.ToLower,
	"ToUpper":     strings.ToUpper,
	"Match":       regexMatch,
}

func regexMatch(pattern string, s string) bool {
	b, _ := regexp.MatchString(pattern, s)
	return b
}

// expandLink returns the expanded long URL to redirect to, executing any
// embedded templates with env data.
//
// If long does not include templates, the default behavior is to append
// env.Path to long.
func expandLink(long string, env expandEnv) (*url.URL, error) {
	if !strings.Contains(long, "{{") {
		// default behavior is to append remaining path to long URL
		if strings.HasSuffix(long, "/") {
			long += "{{.Path}}"
		} else {
			long += "{{with .Path}}/{{.}}{{end}}"
		}
	}
	tmpl, err := texttemplate.New("").Funcs(expandFuncMap).Parse(long)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	if err := tmpl.Execute(buf, env); err != nil {
		return nil, err
	}

	u, err := url.Parse(buf.String())
	if err != nil {
		return nil, err
	}
	mergeQuery(u, env.query)
	return u, nil
}

// resolveTarget returns the URL a link points to for a particular request.
//
// A link's destination is used exactly as written: nothing is appended to it
// and it never reaches the template engine. A path below the link's name goes
// through its pattern instead, which receives that path as .Path. A link with
// a pattern and no destination expands the pattern for its bare name too,
// with an empty .Path.
func resolveTarget(link *Link, env expandEnv) (*url.URL, error) {
	if env.Path == "" && link.Long != "" {
		u, err := url.Parse(link.Long)
		if err != nil {
			return nil, err
		}
		mergeQuery(u, env.query)
		return u, nil
	}
	if link.Pattern == "" {
		return nil, fmt.Errorf("link %q has no destination", link.Short)
	}
	return expandLink(link.Pattern, env)
}

// mergeQuery adds the query parameters of the original request to u.
func mergeQuery(u *url.URL, requestQuery url.Values) {
	if len(requestQuery) == 0 {
		return
	}
	query := u.Query()
	for key, values := range requestQuery {
		for _, v := range values {
			query.Add(key, v)
		}
	}
	u.RawQuery = query.Encode()
}

func devMode() bool { return *dev != "" }

const peerCapName = "tailscale.com/cap/golink"

type capabilities struct {
	Admin bool `json:"admin"`
}

type user struct {
	login   string
	isAdmin bool
	// groups the user belongs to, if the identity source reports any. WhoIs
	// does not, so this is only populated for identity sources that do.
	groups []string
}

// currentUser returns the Tailscale user associated with the request.
// In most cases, this will be the user that owns the device that made the request.
// For tagged devices, the value "tagged-devices" is returned.
// If the user can't be determined (such as requests coming through a subnet router),
// an error is returned unless the -allow-unknown-users flag is set.
//
// When running as a Tailscale Service, authentication is handled via HTTP headers
// automatically injected by tsnet's internal proxy (Tailscale-User-Login, etc.).
// For regular mode, authentication uses WhoIs with the connection's RemoteAddr.
var currentUser = func(r *http.Request) (user, error) {
	if devMode() {
		return user{login: "foo@example.com"}, nil
	}

	// When running as a Tailscale Service, identity headers are automatically injected
	// by tsnet's internal proxy. Restrict this authentication check to cases
	// when we are running in service mode, and the immediate client connection is
	// on loopback.
	if trustIdentityHeaders(r) {
		headerUser := extractUserFromHeaders(r)
		if headerUser.login != "" {
			return headerUser, nil
		}
	}

	// Regular mode: use WhoIs with RemoteAddr
	whois, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		if *allowUnknownUsers {
			// Don't report the error if we are allowing unknown users.
			return user{}, nil
		}
		return user{}, err
	}
	login := whois.UserProfile.LoginName
	caps, _ := tailcfg.UnmarshalCapJSON[capabilities](whois.CapMap, peerCapName)
	for _, cap := range caps {
		if cap.Admin {
			return user{login: login, isAdmin: true}, nil
		}
	}
	return user{login: login}, nil
}

// trustIdentityHeaders returns whether we should trust identity headers injected by tsnet's internal proxy.
var trustIdentityHeaders = func(r *http.Request) bool {
	remoteHost := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteHost); err == nil {
		remoteHost = host
	}
	remoteIP := net.ParseIP(remoteHost)

	return *serviceName != "" && remoteIP != nil && remoteIP.IsLoopback()
}

// extractUserFromHeaders extracts the user from HTTP headers injected by tsnet's internal proxy.
var extractUserFromHeaders = func(r *http.Request) user {
	if tsLogin := r.Header.Get("Tailscale-User-Login"); tsLogin != "" {
		// Look for a peer from x-forwarded-for header. We'll use that for the
		// whois/capmap lookup first.
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			// serve.go sets this to a single address, so we can just
			// trim whitespace and use it.
			ip := strings.TrimSpace(xff)

			// only accept well-formed IP addresses
			if net.ParseIP(ip) == nil {
				log.Printf("invalid IP in X-Forwarded-For header: %q", ip)
				return user{login: tsLogin}
			}

			whois, err := whoisFunc(r.Context(), ip)

			if err != nil {
				log.Printf("WhoIs lookup for IP %q: %v", ip, err)
				return user{login: tsLogin}
			}

			caps, _ := tailcfg.UnmarshalCapJSON[capabilities](whois.CapMap, peerCapName)

			for _, cap := range caps {
				if cap.Admin {
					return user{login: tsLogin, isAdmin: true}
				}
			}

		}

		// If we can't determine admin status, just return the user without admin privileges
		// This allows the service to continue functioning even if the lookup fails
		return user{login: tsLogin}
	}
	return user{}
}

// proxyUser returns the user identified by the headers that an authenticating
// proxy in front of golink sets. Run sets it as currentUser when
// -auth-email-header is given.
//
// golink cannot distinguish a header set by that proxy from one set by whoever
// made the request, so this is only safe where nothing but the proxy can reach
// golink, and the proxy sets the headers on every request it forwards rather
// than passing through what it was given.
func proxyUser(r *http.Request) (user, error) {
	login := strings.TrimSpace(r.Header.Get(*authEmailHeader))
	if login == "" {
		if *allowUnknownUsers {
			// Don't report the error if we are allowing unknown users.
			return user{}, nil
		}
		return user{}, fmt.Errorf("no %s header: golink was reached without going through the authenticating proxy, or the proxy is not configured to set it", *authEmailHeader)
	}

	u := user{login: login}
	if *authGroupsHeader != "" {
		// Proxies differ on whether they send one comma-separated header or
		// repeat the header per group, so accept either.
		for _, value := range r.Header.Values(*authGroupsHeader) {
			for _, group := range strings.Split(value, ",") {
				if group = strings.TrimSpace(group); group != "" {
					u.groups = append(u.groups, group)
				}
			}
		}
	}
	return u, nil
}

// requestUser returns the user making the request: whoever currentUser reports,
// plus admin rights held by their login or by one of their groups in the Admins
// table. The table is an additional source of admins alongside the tailnet ACL
// grant, and grants nothing until an operator adds rows to it.
func requestUser(r *http.Request) (user, error) {
	u, err := currentUser(r)
	if err != nil || u.isAdmin || (u.login == "" && len(u.groups) == 0) {
		return u, err
	}
	admin, err := db.IsAdmin(u.login, u.groups)
	if err != nil {
		// Fail closed: a user we cannot confirm to be an admin is not one.
		log.Printf("looking up admin %q: %v", u.login, err)
		return u, nil
	}
	u.isAdmin = admin
	return u, nil
}

// whoisFunc is a variable so it can be overridden in tests. By default, it calls localClient.WhoIs.
var whoisFunc = func(ctx context.Context, ip string) (*apitype.WhoIsResponse, error) {
	return localClient.WhoIs(ctx, ip)
}

// userExists returns whether a user exists with the specified login in the current tailnet.
func userExists(ctx context.Context, login string) (bool, error) {
	const userTaggedDevices = "tagged-devices" // owner of tagged devices

	if login == userTaggedDevices {
		return false, nil
	}

	if devMode() {
		// in dev mode, just assume the user exists
		return true, nil
	}
	st, err := localClient.Status(ctx)
	if err != nil {
		return false, err
	}
	for _, user := range st.User {
		if user.LoginName == userTaggedDevices {
			continue
		}
		if user.LoginName == login {
			return true, nil
		}
	}
	return false, nil
}

// reShortName matches a valid short name: one or more slash-separated
// segments, each starting with a letter or number. Requiring that first
// character of every segment keeps a name from colliding with the internal
// URLs, which all begin with a dot.
var reShortName = regexp.MustCompile(`^\w[\w\-\.]*(/\w[\w\-\.]*)*$`)

func serveDelete(w http.ResponseWriter, r *http.Request) {
	if *readonly {
		http.Error(w, "golink is in read-only mode", http.StatusMethodNotAllowed)
		return
	}
	short := strings.TrimPrefix(r.URL.Path, "/.delete/")
	if short == "" {
		http.Error(w, "short required", http.StatusBadRequest)
		return
	}

	cu, err := requestUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}

	if !canEditLink(r.Context(), link, cu) {
		http.Error(w, fmt.Sprintf("cannot delete link owned by %q", link.Owner), http.StatusForbidden)
		return
	}

	// Deletion by CLI has never worked because it has always required the XSRF
	// token. (Refer to commit c7ac33d04c33743606f6224009a5c73aa0b8dec0.) If we
	// want to enable deletion via CLI and to honor allowUnknownUsers for
	// deletion, we could change the below to a call to isRequestAuthorized. For
	// now, always require the XSRF token, thus maintaining the status quo.
	if !xsrftoken.Valid(r.PostFormValue("xsrf"), xsrfKey, cu.login, link.Short) {
		http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		return
	}

	if err := db.Delete(short); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deleteLinkStats(link)

	deleteTmpl.Execute(w, deleteData{
		Short:   link.Short,
		Long:    link.Long,
		Pattern: link.Pattern,
		XSRF:    xsrftoken.Generate(xsrfKey, cu.login, newShortName),
	})
}

// serveSave handles requests to save or update a Link.  Both short name and
// long URL are validated for proper format. Existing links may only be updated
// by their owner.
func serveSave(w http.ResponseWriter, r *http.Request) {
	if *readonly {
		http.Error(w, "golink is in read-only mode", http.StatusMethodNotAllowed)
		return
	}
	short, long, pattern := r.FormValue("short"), r.FormValue("long"), r.FormValue("pattern")
	if pattern == "" && strings.Contains(long, "{{") {
		// A template in the destination is how a link used to say that it
		// answered for the paths below its name. Keep understanding clients
		// that predate the pattern field.
		long, pattern = legacyPattern(long, true)
	}
	if short == "" || (long == "" && pattern == "") {
		http.Error(w, "short and either long or pattern required", http.StatusBadRequest)
		return
	}
	if !reShortName.MatchString(short) {
		http.Error(w, "short may only contain letters, numbers, dash, and period, in slash-separated segments", http.StatusBadRequest)
		return
	}
	if strings.Contains(long, "{{") {
		http.Error(w, "long is used as written; a template belongs in pattern", http.StatusBadRequest)
		return
	}
	if _, err := texttemplate.New("").Funcs(expandFuncMap).Parse(pattern); err != nil {
		http.Error(w, fmt.Sprintf("pattern contains an invalid template: %v", err), http.StatusBadRequest)
		return
	}

	cu, err := requestUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !canEditLink(r.Context(), link, cu) {
		http.Error(w, fmt.Sprintf("cannot update link owned by %q", link.Owner), http.StatusForbidden)
		return
	}

	// The lock is only updated if the request explicitly says so, so that
	// clients that know nothing about it (the home page create form, the API)
	// leave an existing link's lock alone.
	setLocked := *openLinks && r.FormValue("lockedset") != ""
	locked := r.FormValue("locked") != ""

	// short name to use for XSRF token.
	// For new link creation, the special newShortName value is used.
	// For existing links, the link's short name is used. This intentionally
	// prevents the home page "create" form from overwriting an existing link;
	// to edit an existing link the user must use the detail page edit form
	// which generates a token scoped to that link's short name.
	tokenShortName := newShortName
	if link != nil {
		tokenShortName = link.Short
	}

	if !isRequestAuthorized(r, cu, tokenShortName) {
		if link != nil && isRequestAuthorized(r, cu, newShortName) {
			// The user submitted from the home page create form but the link
			// already exists. Redirect to the detail page so they can edit it
			// intentionally rather than accidentally overwriting it.
			// reShortName has already limited short to characters that need no
			// escaping, and escaping would only turn its slashes into %2F.
			http.Redirect(w, r, "/.detail/"+short+"?exists=1", http.StatusSeeOther)
		} else {
			http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		}
		return
	}

	// allow transferring ownership to valid users. If empty, set owner to
	// current user. With -open-links, keep the existing owner instead: editing
	// someone else's link is normal there, and should not take it from them.
	owner := r.FormValue("owner")
	if owner != "" {
		exists, err := userExists(r.Context(), owner)
		if err != nil {
			log.Printf("looking up tailnet user %q: %v", owner, err)
		}
		if !exists {
			http.Error(w, "new owner not a valid user: "+owner, http.StatusBadRequest)
			return
		}
	} else if *openLinks && link != nil && link.Owner != "" {
		owner = link.Owner
	} else {
		owner = cu.login
	}

	now := time.Now().UTC()
	newLink := false
	// The link is edited in place below, so keep a copy of what it looked
	// like before: the owner it has now is whose permission a lock needs.
	var previous *Link
	if link == nil {
		link = &Link{
			Short:   short,
			Created: now,
		}
		newLink = true
	} else {
		before := *link
		previous = &before
	}
	link.Short = short
	link.Long = long
	link.Pattern = pattern
	link.LastEdit = now
	link.Owner = owner
	if setLocked && locked != link.Locked {
		// Whose permission this needs is the owner the link has now, not the
		// one it is being given: transferring a link away does not hand over
		// the right to lock it on the way out. A link being created has no
		// previous owner, so the one it is about to get decides.
		subject := link
		if previous != nil {
			subject = previous
		}
		if !canLockLink(subject, cu) {
			http.Error(w, lockRefusal(subject), http.StatusForbidden)
			return
		}
		link.Locked = locked
	}
	if err := db.Save(link); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if acceptHTML(r) {
		successTmpl.Execute(w, homeData{Short: short})
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(link)
	}
	// If this is a new link and not an update inc
	if newLink {
		totalLinkCount.Inc()
	}
}

// canEditLink returns whether the specified user has permission to edit link.
// Admin users can edit all links.
// Non-admin users can only edit their own links or links without an active owner.
// If -open-links is set, any user can also edit any link that its owner has
// not locked.
func canEditLink(ctx context.Context, link *Link, u user) bool {
	if *readonly {
		return false
	}
	if link == nil || link.Owner == "" {
		// new or unowned link
		return true
	}

	if ownsLink(link, u) {
		return true
	}

	if *openLinks {
		return !link.Locked
	}

	owned, err := userExists(ctx, link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}
	// Allow editing if the link is currently unowned
	return err == nil && !owned
}

// ownsLink returns whether the specified user owns link, either as its owner
// or as an admin.
func ownsLink(link *Link, u user) bool {
	return u.isAdmin || (link != nil && link.Owner == u.login)
}

// canLockLink returns whether the specified user has permission to change
// link's locked state.
//
// Locking is an admin decision: it takes a link out of the model that
// -open-links puts every other link into, so it does not belong to whoever
// happened to create the link first. With -owner-can-lock the link's owner may
// do it as well.
//
// Without -open-links nobody can, because a lock would say nothing that is not
// already true: every link is its owner's alone in that mode.
func canLockLink(link *Link, u user) bool {
	if !*openLinks {
		return false
	}
	if u.isAdmin {
		return true
	}
	return *ownerCanLock && link != nil && link.Owner == u.login
}

// lockRefusal explains who could have changed a lock that the current user
// could not.
func lockRefusal(link *Link) string {
	if *ownerCanLock && link != nil && link.Owner != "" {
		return fmt.Sprintf("only %q or an admin can lock this link", link.Owner)
	}
	return "only an admin can lock a link"
}

// serveExport prints a snapshot of the link database. Links are JSON encoded
// and printed one per line. This format is used to restore link snapshots on
// startup.
func serveExport(w http.ResponseWriter, _ *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := db.LoadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(links, func(i, j int) bool {
		return links[i].Short < links[j].Short
	})
	encoder := json.NewEncoder(w)
	for _, link := range links {
		if err := encoder.Encode(link); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// serveExportStats prints a snapshot of the stats database table.
//
// Stats are printed in CSV format with three columns: link ID, UNIX timestamp, and click count.
// Each stat line represents the number of clicks in the previous minute.
func serveExportStats(w http.ResponseWriter, _ *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := db.db.Query("SELECT ID, Created, Clicks FROM Stats ORDER BY Created, ID")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		rows.Close()
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}()

	for rows.Next() {
		var id string
		var created int64
		var clicks int
		err := rows.Scan(&id, &created, &clicks)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// id is not permitted to contain commas, so no need to worry about CSV quoting
		fmt.Fprintf(w, "%s,%d,%d\n", id, created, clicks)
	}
}

func restoreLastSnapshot() error {
	bs := bufio.NewScanner(bytes.NewReader(LastSnapshot))
	var restored int
	for bs.Scan() {
		link := new(Link)
		if err := json.Unmarshal(bs.Bytes(), link); err != nil {
			return err
		}
		if link.Short == "" {
			continue
		}
		_, err := db.Load(link.Short)
		if err == nil {
			continue // exists
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := db.Save(link); err != nil {
			return err
		}
		restored++
	}
	if restored > 0 && *verbose {
		log.Printf("Restored %v links.", restored)
	}
	return bs.Err()
}

func resolveLink(link *url.URL) (*url.URL, error) {
	path := link.Path

	// if link was specified as "go/name", it will parse with no scheme or host.
	// Trim "go" prefix from beginning of path.
	if link.Host == "" {
		path = strings.TrimPrefix(path, *hostname)
	}

	l, remainder, err := lookupLink(strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, err
	}
	dst, err := resolveTarget(l, expandEnv{Now: time.Now().UTC(), Path: remainder})
	if err == nil {
		if dst.Host == "" || dst.Host == *hostname {
			dst, err = resolveLink(dst)
		}
	}
	return dst, err
}

func isRequestAuthorized(r *http.Request, u user, short string) bool {
	if *allowUnknownUsers {
		return true
	}
	if r.Header.Get(secHeaderName) != "" {
		return true
	}

	return xsrftoken.Valid(r.PostFormValue("xsrf"), xsrfKey, u.login, short)
}

// parseAdvertiseTags parses a comma-separated list of ACL tags.
// Each tag must start with "tag:". Empty strings are ignored.
func parseAdvertiseTags(s string) ([]string, error) {
	var tags []string
	for _, tag := range strings.Split(s, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if !strings.HasPrefix(tag, "tag:") {
			return nil, fmt.Errorf("invalid advertise tag %q: must start with \"tag:\"", tag)
		}
		tags = append(tags, tag)
	}
	return tags, nil
}
