package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/store"
)

// contentHTML is the media type of the embedded pages.
const contentHTML = "text/html; charset=utf-8"

// chartPoints is how many of the newest results the detail-page sparkline
// plots. 200 keeps the SVG readable while covering a useful slice of history.
const chartPoints = 200

// Sparkline geometry, in SVG user units. The polyline is drawn inside a
// chartPad margin on every side, so the curve never touches the frame.
const (
	chartWidth  = 800
	chartHeight = 120
	chartPad    = 10
)

// uptime24hWindow and uptime7dWindow are the detail page's uptime windows; they
// match the windows /api/status reports.
const (
	uptime24hWindow = 24 * time.Hour
	uptime7dWindow  = 7 * 24 * time.Hour
)

// indexHTML is the single-page status screen, inlined into the binary so the
// binary is self-contained and the page needs no external assets.
//
//go:embed index.html
var indexHTML []byte

// detailHTML is the per-target detail page: a server-rendered card with the
// latest outcome, uptime figures, certificate expiry, and an SVG sparkline of
// recent durations. It needs no JavaScript at all and references no external
// resources.
//
//go:embed detail.html
var detailHTML []byte

// detailTmpl is parsed once at startup. html/template escapes every substituted
// value for its HTML context, so a target name or URL from configuration can
// never inject markup.
var detailTmpl = template.Must(template.New("detail").Parse(string(detailHTML)))

// server holds the dependencies of the web handlers.
type server struct {
	targets []config.Target
	byName  map[string]config.Target
	store   *store.Store
}

// New returns the HTTP handler that serves the status screen and the per-target
// detail pages.
//
// GET / serves the dashboard; GET /targets/{name} serves the detail page for a
// configured target. Unknown names are a 404 because configuration, not the
// store, owns the target list. Every other path keeps the mux default 404. The
// handler is safe for concurrent use.
func New(cfg config.Config, st *store.Store) http.Handler {
	s := &server{
		targets: cfg.Targets,
		byName:  make(map[string]config.Target, len(cfg.Targets)),
		store:   st,
	}
	for _, t := range cfg.Targets {
		s.byName[t.Name] = t
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /targets/{name}", s.handleDetail)
	return mux
}

// handleIndex writes the embedded dashboard.
func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", contentHTML)
	_, _ = w.Write(indexHTML)
}

// handleDetail renders the detail page of one configured target.
func (s *server) handleDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	t, ok := s.byName[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.store == nil {
		slog.Error("web: detail requested without a store", "target", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data, err := s.detailData(t)
	if err != nil {
		slog.Error("web: build target detail", "target", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentHTML)
	// The page is rendered straight into the response; a failure part-way
	// through can only be a disconnected client, which the header write above
	// has already committed to, so there is nothing left to report.
	if err := detailTmpl.Execute(w, data); err != nil {
		slog.Error("web: render target detail", "target", name, "err", err)
	}
}

// detailData is the detail template's view model. Uptime and certificate values
// are pre-formatted so the template stays free of logic.
type detailData struct {
	Name string
	URL  string

	HasLast        bool
	OutcomeLabel   string
	OutcomeClass   string
	LastAt         string
	LastDurationMs int64
	StatusCode     int
	Error          string

	Uptime24h string
	Uptime7d  string

	HasCert  bool
	CertDays int64
	CertWarn bool

	HasChart    bool
	ChartWidth  int
	ChartHeight int
	Points      string
}

// detailData assembles the view model for one target.
func (s *server) detailData(t config.Target) (detailData, error) {
	d := detailData{
		Name:         t.Name,
		URL:          t.URL,
		OutcomeLabel: "pending",
		OutcomeClass: "unknown",
		LastAt:       "never checked",
		ChartWidth:   chartWidth,
		ChartHeight:  chartHeight,
	}

	last, ok, err := s.store.Latest(t.Name)
	if err != nil {
		return detailData{}, err
	}
	if ok {
		d.HasLast = true
		d.OutcomeLabel = string(last.Outcome)
		d.OutcomeClass = string(last.Outcome)
		d.LastAt = last.StartedAt.UTC().Format(time.RFC3339)
		d.LastDurationMs = last.Duration.Milliseconds()
		d.StatusCode = last.StatusCode
		d.Error = last.Err
		if !last.CertExpiresAt.IsZero() {
			d.HasCert = true
			d.CertDays = int64(math.Floor(time.Until(last.CertExpiresAt).Hours() / 24))
			// Less than 30 days left gets the amber treatment. The threshold
			// lives here, not in the template.
			d.CertWarn = d.CertDays < 30
		}
	}

	// Uptime is rendered as "12.3%" or, when the window holds no completed
	// check, "no data" rather than a misleading 0%.
	for _, u := range []struct {
		window time.Duration
		dst    *string
	}{
		{uptime24hWindow, &d.Uptime24h},
		{uptime7dWindow, &d.Uptime7d},
	} {
		percent, hasData, err := s.store.Uptime(t.Name, u.window)
		if err != nil {
			return detailData{}, err
		}
		if hasData {
			*u.dst = fmt.Sprintf("%.1f%%", percent)
		} else {
			*u.dst = "no data"
		}
	}

	history, err := s.store.History(t.Name, chartPoints)
	if err != nil {
		return detailData{}, err
	}
	if len(history) > 0 {
		// History is newest first; the sparkline reads oldest on the left.
		oldestFirst := make([]checker.Result, len(history))
		for i, r := range history {
			oldestFirst[len(history)-1-i] = r
		}
		d.HasChart = true
		d.Points = sparklinePoints(oldestFirst)
	}

	return d, nil
}

// sparklinePoints turns a chronological result slice into the SVG "points"
// attribute: x spreads left to right, and y maps the duration onto the padded
// chart height, so the slowest check sits at the top and the fastest at the
// bottom. A flat series (one point, or identical durations) is drawn on the
// middle line rather than dividing by zero.
func sparklinePoints(results []checker.Result) string {
	if len(results) == 0 {
		return ""
	}

	durations := make([]float64, len(results))
	minMs, maxMs := math.Inf(1), math.Inf(-1)
	for i, r := range results {
		d := float64(r.Duration.Milliseconds())
		durations[i] = d
		minMs = math.Min(minMs, d)
		maxMs = math.Max(maxMs, d)
	}

	innerWidth := float64(chartWidth - 2*chartPad)
	innerHeight := float64(chartHeight - 2*chartPad)
	top := float64(chartPad)
	bottom := float64(chartHeight - chartPad)

	points := make([]byte, 0, len(results)*12)
	for i, d := range durations {
		var x float64
		if len(results) == 1 {
			x = float64(chartWidth) / 2
		} else {
			x = float64(chartPad) + float64(i)*innerWidth/float64(len(results)-1)
		}

		y := (top + bottom) / 2 // flat series: the middle line
		if maxMs > minMs {
			y = bottom - (d-minMs)/(maxMs-minMs)*innerHeight
		}

		if i > 0 {
			points = append(points, ' ')
		}
		points = append(points, fmt.Sprintf("%.1f,%.1f", x, y)...)
	}
	return string(points)
}
