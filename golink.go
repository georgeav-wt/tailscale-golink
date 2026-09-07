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
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	texttemplate "text/template"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tailscale/hujson"
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
	mysqlDSN          = flag.String("mysql", os.Getenv("GOLINK_MYSQL_DSN"), "if non-empty, store links in this MySQL database instead of in SQLite, in the form user:password@tcp(host:3306)/golink. Unlike a SQLite file it can be shared by several instances. It holds a password, so give it in the environment as GOLINK_MYSQL_DSN or in -config rather than on the command line")
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
	adminOnlyExport   = flag.Bool("admin-only-export", false, "let only admins export every link at once, and stop offering the export, stats and metrics URLs to anybody else")
	xsrfKeyFlag       = flag.String("xsrf-key", os.Getenv("GOLINK_XSRF_KEY"), "secret the XSRF tokens in forms are signed with, shared by every instance serving the same links; without it each instance invents its own and refuses the forms of the others. It is a credential, so give it in the environment or -config rather than on the command line")
	authEmailHeader   = flag.String("auth-email-header", "", `if non-empty, identify users by this HTTP header, set by an authenticating proxy in front of golink (e.g. "X-Auth-Request-Email"), rather than by their tailnet identity`)
	authGroupsHeader  = flag.String("auth-groups-header", "", `HTTP header holding the comma-separated groups a user belongs to (e.g. "X-Auth-Request-Groups"); only read when -auth-email-header is set`)
	advertiseTags     = flag.String("advertise-tags", os.Getenv("TS_ADVERTISE_TAGS"), "comma-separated list of ACL tags to advertise (e.g. tag:golink)")
	serviceName       = flag.String("register-as-service", envknob.String("TS_SERVICE_NAME"), "register as a Tailscale Service (e.g., svc:golink); requires tagged node")
	configFile        = flag.String("config", "", "path of a file setting any of these options, one per line as \"name\": value, with comments allowed; an option given on the command line wins over the file")
	webhookURL        = flag.String("webhook-url", "", "if non-empty, POST every audit event to this URL; it is a credential, so prefer -config to the command line, where it would be visible in the process list")
	webhookFormat     = flag.String("webhook-format", "slack", `shape of the webhook body: "slack" for a message an incoming webhook renders, or "json" for the audit event itself`)
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
var db *DB

var localClient *local.Client

func Run() error {
	flag.Parse()

	if *configFile != "" {
		if err := loadConfig(flag.CommandLine, *configFile); err != nil {
			return fmt.Errorf("reading %s: %w", *configFile, err)
		}
	}

	if *xsrfKeyFlag != "" {
		if len(*xsrfKeyFlag) < minXSRFKeyLength {
			return fmt.Errorf("-xsrf-key is %d characters; it needs at least %d", len(*xsrfKeyFlag), minXSRFKeyLength)
		}
		xsrfKey = *xsrfKeyFlag
	}

	switch *webhookFormat {
	case "slack", "json":
	default:
		return fmt.Errorf("-webhook-format %q is not one of slack or json", *webhookFormat)
	}
	if *webhookURL != "" {
		startWebhook()
	}

	if *authEmailHeader != "" {
		// Identity comes from a proxy in front of golink, not from the tailnet.
		currentUser = proxyUser
	}

	hostinfo.SetApp("golink")

	// if resolving from backup, set sqlitefile and snapshot flags to
	// restore links into an in-memory sqlite database.
	if *resolveFromBackup != "" {
		*sqlitefile = ":memory:"
		// Resolving a name against a snapshot file needs no stored links, so
		// this does not touch a configured MySQL database.
		*mysqlDSN = ""
		snapshot = resolveFromBackup
		if flag.NArg() != 1 {
			log.Fatal("--resolve-from-backup also requires a link to be resolved")
		}
	}

	if *mysqlDSN == "" && *sqlitefile == "" {
		if devMode() {
			tmpdir, err := os.MkdirTemp("", "golink_dev_*")
			if err != nil {
				return err
			}
			*sqlitefile = filepath.Join(tmpdir, "golink.db")
			log.Printf("Dev mode temp db: %s", *sqlitefile)
		} else {
			return errors.New("--sqlitedb or --mysql is required")
		}
	}

	var err error
	if *mysqlDSN != "" {
		if *sqlitefile != "" {
			return errors.New("-mysql and -sqlitedb each name a database to store links in; give one, not both")
		}
		// The DSN holds a password, so it must not reach a log line. The
		// driver's own errors name the address it failed to reach and not the
		// DSN, and nothing here adds it.
		if db, err = NewMySQLDB(*mysqlDSN); err != nil {
			return fmt.Errorf("connecting to MySQL: %w", err)
		}
	} else if db, err = NewSQLiteDB(*sqlitefile); err != nil {
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
	flushStatsOnShutdown()

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
	Query string
	// Sort is the order the results are in, one of the sortOrders keys.
	Sort    string
	Results []searchResult

	// ShowBulkURLs is whether to offer the link that exports every link.
	ShowBulkURLs bool
}

// SortLink returns the URL of these same results in another order, for the
// column headings to link to.
func (d searchData) SortLink(order string) string {
	values := url.Values{}
	path := "/.all"
	if d.Query != "" {
		path = "/.search"
		values.Set("q", d.Query)
	}
	values.Set("sort", order)
	return path + "?" + values.Encode()
}

// sortOrders are the orders results can be listed in, each with the direction
// that is useful for it: names read alphabetically, while for clicks and dates
// the interesting end is the top.
var sortOrders = map[string]func(a, b searchResult) bool{
	"name":  func(a, b searchResult) bool { return a.Short < b.Short },
	"owner": func(a, b searchResult) bool { return a.Owner < b.Owner || (a.Owner == b.Owner && a.Short < b.Short) },
	"clicks": func(a, b searchResult) bool {
		return a.NumClicks > b.NumClicks || (a.NumClicks == b.NumClicks && a.Short < b.Short)
	},
	"edited": func(a, b searchResult) bool {
		return a.LastEdit.After(b.LastEdit) || (a.LastEdit.Equal(b.LastEdit) && a.Short < b.Short)
	},
}

// sortOrder returns the name of a valid order, defaulting to by name.
func sortOrder(order string) string {
	if _, ok := sortOrders[order]; ok {
		return order
	}
	return "name"
}

// searchResults annotates links with their current click counts (read from the
// live in-memory counter, the same source the home page uses), preserving the
// historical alphabetical ordering by short name.
func searchResults(links []*Link, order string) []searchResult {
	stats.mu.Lock()
	results := make([]searchResult, len(links))
	for i, link := range links {
		results[i] = searchResult{Link: link, NumClicks: stats.clicks[link.Short]}
	}
	stats.mu.Unlock()

	less := sortOrders[sortOrder(order)]
	sort.Slice(results, func(i, j int) bool { return less(results[i], results[j]) })
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
	// Suggestions are links resembling the name that was asked for and did
	// not exist, offered above the form that would create it.
	Suggestions []*Link

	// Pattern and AddedScheme are for the page shown after saving: what the
	// link now points at, and whether a scheme had to be supplied to get there.
	Pattern     string
	AddedScheme bool
}

// deleteData is the data used by deleteTmpl.
type deleteData struct {
	Short   string
	Long    string
	Pattern string
	XSRF    string
}

// xsrfKey signs the tokens that say a form came from this service rather than
// from somewhere else. It is generated per process, which is right for one
// process and wrong for several: a form rendered by one instance would be
// refused by any other. -xsrf-key replaces it with a shared secret.
var xsrfKey string

// minXSRFKeyLength is the shortest shared key worth accepting. The generated
// one is 24 random bytes; a short one weakens every token signed with it, so a
// mistyped or truncated secret is refused rather than quietly used.
const minXSRFKeyLength = 16

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
	"go": func() string { return linkHostname() },
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
	count, err := db.LinkCount()
	if err != nil {
		return err
	}
	totalLinkCount.Set(float64(count))

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

	if len(stats.dirty) > 0 {
		if err := db.SaveStats(stats.dirty); err != nil {
			return err
		}
		stats.dirty = make(ClickStats)
	}

	// Read the totals back -- always, not only after a save -- so that an
	// instance counts the clicks the others have recorded as well as its own.
	// Without this each one would show only what it had seen since it started,
	// and two instances would disagree about which links are popular; an
	// instance that happens to be receiving no traffic would never catch up at
	// all. The stored rows are increments, so the sum is right however many
	// instances there are, and a link deleted elsewhere drops out of this list
	// too, because deleting one deletes its rows.
	clicks, err := db.LoadStats()
	if err != nil {
		return err
	}
	stats.clicks = clicks
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

// flushStatsOnShutdown writes the clicks counted since the last flush when the
// process is asked to stop, so that a rolling restart does not throw away a
// minute of them for every instance it replaces.
func flushStatsOnShutdown() {
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopping
		if err := flushStats(); err != nil {
			log.Printf("flushing stats before stopping: %v", err)
		}
		// Having done the one thing worth doing, stop the way an unhandled
		// signal would have.
		signal.Reset(sig.(syscall.Signal))
		syscall.Kill(syscall.Getpid(), sig.(syscall.Signal))
	}()
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
		Short:       short,
		Long:        long,
		Clicks:      clicks,
		XSRF:        xsrftoken.Generate(xsrfKey, cu.login, newShortName),
		ReadOnly:    *readonly,
		User:        cu.login,
		Suggestions: suggestLinks(short),
	})
}

func serveAll(w http.ResponseWriter, r *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := db.LoadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cu, _ := requestUser(r)
	order := sortOrder(r.URL.Query().Get("sort"))
	searchTmpl.Execute(w, searchData{
		Sort:         order,
		Results:      searchResults(links, order),
		ShowBulkURLs: showBulkURLs(cu),
	})
}

// helpData is the data used by the helpTmpl template.
type helpData struct {
	// ShowBulkURLs is whether to describe the URLs that read the whole link
	// set at once. They keep working for anyone when -admin-only-export is not
	// set; this only decides whether they are offered.
	ShowBulkURLs bool
}

func serveHelp(w http.ResponseWriter, r *http.Request) {
	// A failure to say who this is means they are not an admin, which is the
	// safe way round and leaves the rest of the page readable.
	cu, _ := requestUser(r)
	helpTmpl.Execute(w, helpData{ShowBulkURLs: showBulkURLs(cu)})
}

// showBulkURLs reports whether to offer the URLs that read every link at once.
func showBulkURLs(u user) bool {
	return !*adminOnlyExport || u.isAdmin
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

// maxSuggestions is how many near misses are worth offering; more than a few
// and reading them is slower than typing the name again.
const maxSuggestions = 5

// suggestLinks returns the links whose names most resemble short, for a name
// that was asked for and does not exist. It returns nothing for an empty name,
// which is the home page rather than a link that was missed.
func suggestLinks(short string) []*Link {
	if short == "" {
		return nil
	}
	links, err := db.LoadAll()
	if err != nil {
		log.Printf("loading links to suggest for %q: %v", short, err)
		return nil
	}

	// Compare normalised names, so that a suggestion differs from what was
	// asked for in the way that resolving a link would notice, rather than in
	// case or in dashes, which it would not.
	want := linkID(short)
	type scored struct {
		link  *Link
		score int
	}
	var candidates []scored
	for _, link := range links {
		id := linkID(link.Short)
		if id == want {
			continue // it exists after all; not a miss
		}
		score := -1
		switch {
		case strings.HasPrefix(id, want) || strings.HasPrefix(want, id):
			// A name typed short, or one segment of a longer name: asking for
			// go/gh when go/gh/infra exists, or the other way around.
			score = 0
		case editDistance(id, want) <= 1:
			score = 1
		case len(want) >= 4 && editDistance(id, want) <= 2:
			score = 2
		case sameFirstSegment(link.Short, short):
			// A sibling under the same name: go/team/nothing suggests the
			// other links under go/team.
			score = 3
		case strings.Contains(id, want) || strings.Contains(want, id):
			score = 4
		}
		if score >= 0 {
			candidates = append(candidates, scored{link, score})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].link.Short < candidates[j].link.Short
	})
	if len(candidates) > maxSuggestions {
		candidates = candidates[:maxSuggestions]
	}

	suggestions := make([]*Link, 0, len(candidates))
	for _, c := range candidates {
		suggestions = append(suggestions, c.link)
	}
	return suggestions
}

// sameFirstSegment returns whether two names share a first path segment, and
// have more than that one segment between them.
func sameFirstSegment(a, b string) bool {
	first, _, aMore := strings.Cut(a, "/")
	second, _, bMore := strings.Cut(b, "/")
	if !aMore && !bMore {
		return false
	}
	return linkID(first) == linkID(second)
}

// editDistance returns the number of single-character insertions, deletions
// and substitutions that turn a into b.
func editDistance(a, b string) int {
	// Only the previous row of the matrix is needed to compute the next.
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, min(current[j-1]+1, previous[j-1]+cost))
		}
		previous, current = current, previous
	}
	return previous[len(b)]
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

	cu, _ := requestUser(r)
	order := sortOrder(r.URL.Query().Get("sort"))
	searchTmpl.Execute(w, searchData{
		Query:        query,
		Sort:         order,
		Results:      searchResults(links, order),
		ShowBulkURLs: showBulkURLs(cu),
	})
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

// withScheme returns a destination with a scheme, assuming https when one was
// left off.
//
// A destination without a scheme is a relative URL, which a browser resolves
// against golink itself, so a link pointing at "g.co/test" lands the visitor on
// http://go/g.co/test rather than on Google. Someone writing that meant the
// scheme to be there.
//
// Two kinds of destination are left alone. One beginning with "/" is how a link
// aliases another link, which resolveLink follows on purpose. One beginning with
// a template decides its own scheme when it is expanded, and there is nothing
// here to inspect.
//
// A destination that really is served over plain http still works: write the
// http:// out and this leaves it alone.
func withScheme(dest string) string {
	if dest == "" || strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "{{") {
		return dest
	}
	if hasScheme(dest) {
		return dest
	}
	return "https://" + dest
}

// rePort matches what follows the colon in a host and port, as opposed to what
// follows the colon in a scheme.
var rePort = regexp.MustCompile(`^[0-9]+(/|$)`)

// hasScheme reports whether dest begins with a URI scheme. It is not enough to
// look for a colon: "localhost:8080/x" is a host and a port, and means to be
// fetched over https, while "mailto:someone@example.com" is a scheme and is not
// a URL to fetch at all.
func hasScheme(dest string) bool {
	scheme, rest, ok := strings.Cut(dest, ":")
	if !ok || scheme == "" {
		return false
	}
	for i, r := range scheme {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return !rePort.MatchString(rest)
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

// linkHostname is the host that go links are written with, in the UI and
// anywhere else golink names one.
func linkHostname() string {
	if devMode() {
		// in dev mode, just use "go" instead of "localhost:8080"
		return defaultHostname
	}
	return *hostname
}

// loadConfig applies a configuration file to a set of flags. The file names
// the same options the command line does, so that anything gettable from
// -help is settable in the file and a new option needs nothing added here:
//
//	{
//	    // Comments and trailing commas are allowed.
//	    "open-links": true,
//	    "sqlitedb": "/home/nonroot/golink.db",
//	}
//
// An option given on the command line wins over the file, which is what makes
// a file of settled defaults and a one-off override work together. A name the
// flags do not know is an error rather than something to ignore, since the
// whole point of the file is to be the place options are written down, and a
// misspelling there would otherwise be silent.
func loadConfig(fs *flag.FlagSet, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// hujson is JSON with comments and trailing commas, which a file of
	// settings wants and which JSON refuses.
	b, err = hujson.Standardize(b)
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		return err
	}

	// Anything named on the command line stays as it was given there.
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	for _, name := range slices.Sorted(maps.Keys(settings)) {
		if name == "config" {
			return errors.New(`a configuration file cannot name another one ("config")`)
		}
		if fs.Lookup(name) == nil {
			return fmt.Errorf("no such option %q", name)
		}
		if given[name] {
			continue
		}
		value, err := settingValue(settings[name])
		if err != nil {
			return fmt.Errorf("option %q: %w", name, err)
		}
		if err := fs.Set(name, value); err != nil {
			return fmt.Errorf("option %q: %w", name, err)
		}
	}
	return nil
}

// settingValue renders a value from a configuration file the way the flag
// package would have received it on the command line.
func settingValue(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case nil:
		return "", errors.New("has no value")
	default:
		return "", fmt.Errorf("is a %T, which is not a setting", v)
	}
}

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
	logAudit("delete", cu, link, nil)

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
	if r.FormValue("dynamicset") != "" {
		// The form carries a "dynamic link" checkbox and has spoken. Unticked
		// means the link has no pattern, whatever is still sitting in the
		// pattern field: it is hidden with CSS rather than disabled, so it is
		// submitted either way.
		if r.FormValue("dynamic") == "" {
			pattern = ""
		}
	} else if pattern == "" && strings.Contains(long, "{{") {
		// Nothing said, and there is a template in the destination, which is
		// how a link used to say that it answered for the paths below its
		// name. Keep understanding clients that predate the pattern field.
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

	// Supply a scheme that was left off, and say so afterwards rather than
	// changing what somebody wrote without telling them.
	written := long + pattern
	long, pattern = withScheme(long), withScheme(pattern)
	addedScheme := long+pattern != written

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
	// The link is edited in place below, so the audit log needs its own copy
	// of what it looked like before.
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
	link.LastEditBy = cu.login
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

	if newLink {
		logAudit("create", cu, link, nil)
	} else {
		logAudit("update", cu, link, previous)
	}

	if acceptHTML(r) {
		successTmpl.Execute(w, homeData{
			Short:       short,
			Long:        link.Long,
			Pattern:     link.Pattern,
			AddedScheme: addedScheme,
		})
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

// auditWriter is where the audit log goes. Stdout, so that whatever collects
// container output ships it, leaving golink with no idea where the lines end
// up and nothing to retry. golink's operational logging stays on stderr, so
// the two do not have to be told apart.
//
// It is a variable so tests can read what was written.
var auditWriter io.Writer = os.Stdout

// auditMu serialises audit lines, so that two requests cannot interleave
// halves of a JSON object.
var auditMu sync.Mutex

// auditEntry is one line of the audit log. The first few fields are named the
// way log collectors expect to find them, so that a line arrives as a parsed
// event rather than a wall of text.
type auditEntry struct {
	Timestamp time.Time  `json:"timestamp"`
	Status    string     `json:"status"`
	Service   string     `json:"service"`
	Message   string     `json:"message"`
	Action    string     `json:"action"`
	Short     string     `json:"short"`
	User      string     `json:"user"`
	Link      *auditLink `json:"link,omitempty"`
	Previous  *auditLink `json:"previous,omitempty"`
}

// auditLink is the part of a Link worth recording a change to.
type auditLink struct {
	Long    string `json:"long,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Locked  bool   `json:"locked,omitempty"`
}

func newAuditLink(link *Link) *auditLink {
	if link == nil {
		return nil
	}
	return &auditLink{Long: link.Long, Pattern: link.Pattern, Owner: link.Owner, Locked: link.Locked}
}

// logAudit records a change to a link. Changes made any other way -- restoring
// a snapshot, or editing the database by hand -- are not recorded, since they
// do not pass through here.
func logAudit(action string, u user, link, previous *Link) {
	who := u.login
	if who == "" {
		who = "unknown"
	}
	entry := auditEntry{
		Timestamp: time.Now().UTC(),
		Status:    "info",
		Service:   "golink",
		Message:   fmt.Sprintf("%s %s/%s by %s", action, defaultHostname, link.Short, who),
		Action:    action,
		Short:     link.Short,
		User:      who,
		Link:      newAuditLink(link),
		Previous:  newAuditLink(previous),
	}

	auditMu.Lock()
	if err := json.NewEncoder(auditWriter).Encode(entry); err != nil {
		log.Printf("writing audit log: %v", err)
	}
	auditMu.Unlock()

	notifyWebhook(entry)
}

// webhookEvents carries audit events to the goroutine that posts them. Saving
// a link never waits on somebody else's HTTP server, and never fails because
// of it: an audit trail that can break the thing it audits is worse than one
// with a hole in it, and a hole is logged when it happens.
var webhookEvents chan auditEntry

// webhookDone is closed once the sender has finished the events it was given.
var webhookDone chan struct{}

// webhookQueueLength is how far the sender may fall behind before events are
// dropped. Links are edited by hand, so this is minutes of the busiest
// imaginable day.
const webhookQueueLength = 64

// startWebhook begins posting audit events to -webhook-url.
func startWebhook() {
	webhookEvents = make(chan auditEntry, webhookQueueLength)
	webhookDone = make(chan struct{})

	go func() {
		defer close(webhookDone)
		client := &http.Client{Timeout: 10 * time.Second}
		for entry := range webhookEvents {
			if err := postWebhook(client, entry); err != nil {
				log.Printf("posting %s of %s to the webhook: %v", entry.Action, entry.Short, err)
			}
		}
	}()
}

// stopWebhook finishes the events already queued and stops the sender. Nothing
// but a test needs it; the process otherwise runs until it is killed.
func stopWebhook() {
	if webhookEvents == nil {
		return
	}
	close(webhookEvents)
	<-webhookDone
	webhookEvents = nil
}

// notifyWebhook hands an audit event to the sender, or drops it if the sender
// is far enough behind that keeping it would mean growing without bound.
func notifyWebhook(entry auditEntry) {
	if webhookEvents == nil {
		return
	}
	select {
	case webhookEvents <- entry:
	default:
		log.Printf("dropping %s of %s: the webhook is not keeping up", entry.Action, entry.Short)
	}
}

// postWebhook sends one audit event.
//
// No error it returns names the URL. A Slack webhook URL is a credential, and
// the errors of net/http carry the URL they were given, so they are unwrapped
// before they can be logged.
func postWebhook(client *http.Client, entry auditEntry) error {
	body, err := webhookBody(entry)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", *webhookURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("the webhook URL cannot be requested")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			return urlErr.Err
		}
		return errors.New("the request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Slack answers a refusal with a short reason, such as no_service.
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(reply)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// webhookBody renders an audit event in the shape -webhook-format asks for.
func webhookBody(entry auditEntry) ([]byte, error) {
	if *webhookFormat == "json" {
		return json.Marshal(entry)
	}
	return json.Marshal(map[string]string{"text": slackText(entry)})
}

// slackText renders an audit event as one line of Slack markup: what happened,
// to which link, by whom, and where the link points now.
func slackText(entry auditEntry) string {
	var b strings.Builder
	name := entry.Short
	fmt.Fprintf(&b, "*%s* <http://%s/.detail/%s|%s/%s> by %s",
		entry.Action, linkHostname(), name, linkHostname(), slackEscape(name), slackEscape(entry.User))

	if to := auditLinkText(entry.Link); to != "" {
		fmt.Fprintf(&b, "\n%s", to)
	}
	if from := auditLinkText(entry.Previous); from != "" && from != auditLinkText(entry.Link) {
		fmt.Fprintf(&b, "\n_was_ %s", from)
	}
	if entry.Link != nil && entry.Previous != nil && entry.Link.Locked != entry.Previous.Locked {
		if entry.Link.Locked {
			b.WriteString("\n_locked_")
		} else {
			b.WriteString("\n_unlocked_")
		}
	}
	return b.String()
}

// auditLinkText describes where a link pointed, in the two fields it has.
func auditLinkText(l *auditLink) string {
	switch {
	case l == nil:
		return ""
	case l.Long != "" && l.Pattern != "":
		return slackEscape(l.Long) + " (pattern " + slackEscape(l.Pattern) + ")"
	case l.Pattern != "":
		return "pattern " + slackEscape(l.Pattern)
	}
	return slackEscape(l.Long)
}

// slackEscape escapes the three characters Slack reads as markup. A
// destination is full of ampersands.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// serveExport prints a snapshot of the link database. Links are JSON encoded
// and printed one per line. This format is used to restore link snapshots on
// startup.
func serveExport(w http.ResponseWriter, r *http.Request) {
	if *adminOnlyExport {
		// Every link in one request is a different thing from looking one up,
		// so it can be held to a different rule.
		cu, err := requestUser(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !cu.isAdmin {
			http.Error(w, "only an admin can export every link", http.StatusForbidden)
			return
		}
	}

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

	err := db.StatRows(func(id string, created int64, clicks int) error {
		// id is not permitted to contain commas, so no need to worry about CSV quoting
		fmt.Fprintf(w, "%s,%d,%d\n", id, created, clicks)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
