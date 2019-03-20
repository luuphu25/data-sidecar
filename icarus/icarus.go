// Icarus solves a problem I've noticed with prometheus.
// It has a lot of opinions which might not suit every
// use case it's capable of ingesting. Not regarding
// this obvious safeguard in the proper context, we continue.

package icarus

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luuphu25/data-sidecar/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

var (
	icarusReturnSize = prometheus.NewSummary(prometheus.SummaryOpts{
		Name: "icarus_return_size_summary",
		Help: "How much is being served",
	})
	icarusReturnMetrics = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "icarus_return_metrics_summary",
		Help: "How many metrics being served",
	}, []string{"type"})
	icarusRequestCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "icarus_request_counter",
		Help: "How many requests are coming in?",
	})
	icarusErrorCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "icarus_error_counter",
		Help: "How many processing errors in icarus?",
	}, []string{"type"})
	errRead = errors.New("Not found")
)

func init() {
	prometheus.MustRegister(icarusRequestCounter)
	prometheus.MustRegister(icarusReturnMetrics)
	prometheus.MustRegister(icarusReturnSize)
	prometheus.MustRegister(icarusErrorCounter)
}

// ServePage holds a linked list of pages to serve over http.
type ServePage struct {
	*sync.RWMutex
	Page string
	Link *ServePage
}

// NewServePage generates a linked list of pages to serve.
func NewServePage() *ServePage {
	var mux sync.RWMutex
	out := ServePage{&mux, "", nil}
	out.Link = &out
	return &out
}

// AddPage adds another page to serve.
func (s *ServePage) AddPage() {
	s.Lock()
	defer s.Unlock()
	other := NewServePage()
	sNext := s.Link
	s.Link = other
	other.Link = sNext
}

// Next advances the servepage list.
func (s *ServePage) Next() *ServePage {
	return s.Link
}

func (s *ServePage) Write(inp string) {
	s.Lock()
	defer s.Unlock()
	s.Page = inp
}

func (s *ServePage) Read() string {
	s.RLock()
	defer s.RUnlock()
	return s.Page
}

// Icarus is like a prometheus store except it's easy to hurt yourself with.
type Icarus struct {
	*sync.Mutex
	Store  *IcarusStore
	Ticker *time.Ticker
	Chan   chan util.Metric
	prefix string
	serve  *ServePage
}

// NewIcarus builds and starts an icarus process.
func NewIcarus(prefix string) *Icarus {
	var mux sync.Mutex
	// Only really need two pages.
	sp := NewServePage()
	sp.AddPage()
	ticker := time.NewTicker(10 * time.Second)
	i := Icarus{&mux, NewRollingStore(2), ticker,
		make(chan util.Metric, 1), prefix, sp}
	go (&i).start()
	go (&i).rollStore()
	return &i
}

// startIcarus makes and reads from the channel that will run the whole operation
func (i *Icarus) start() {
	for x := range i.Chan {
		name := "unnamed_metric"
		if val, ok := x.Desc["__name__"]; ok && (len(val) > 0) {
			name = val
		}
		x.Desc["__name__"] = i.prefix + name
		i.Store.Insert(x)
	}
}

// Record puts things into the icarus channel.
func (i *Icarus) Record(x util.Metric) {
	i.Chan <- x
}

// Finish does nothing
func (u *Icarus) Finish() {}

// rollStore moves the metric store to the old metric store after obliterating the latter
func (i *Icarus) rollStore() {
	ii := 0
	for _ = range i.Ticker.C {
		//10 seconds -> minute
		ii = (ii + 1) % 6
		i.rollup()
		if ii == 0 {
			i.rollStoreBusiness()
		}
	}
}

func (i *Icarus) rollStoreBusiness() {
	i.Lock()
	defer i.Unlock()
	i.Store.Roll()
}

// MetricToProm changes a map into a string.
func MetricToProm(met util.Metric) string {
	name := met.Desc["__name__"]
	kvprune := make(map[string]string)
	for key, val := range met.Desc {
		if (key == "_hash") || (key == "__name__") || (val == "") || (key == "ft_target") {
			continue
		}
		kvprune[key] = val
	}
	sorted := make([]string, len(kvprune))
	index := 0
	for key := range kvprune {
		sorted[index] = key
		index++
	}
	out := make([]string, len(sorted))
	sort.Strings(sorted)
	for ii, xx := range sorted {
		out[ii] = xx + "=\"" + met.Desc[xx] + "\""
	}
	return name + "{" + strings.Join(out, ",") + "} " + strconv.FormatFloat(met.Data.Val, 'f', -1, 32) + "\n"
}

// rollup prepares the local store for emission.
func (i *Icarus) rollup() {
	i.Lock()
	defer i.Unlock()
	useBuffer := bytes.NewBuffer([]byte("\n# These metrics generated by icarus.\n"))
	useMets := i.Store.Dump()
	metrics := 0
	// whatever the work item level is, the metric name, the anomalies
	for _, val := range useMets {
		if !math.IsNaN(val.Data.Val) {
			metrics++
			useBuffer.Write([]byte(MetricToProm(val)))
		}
	}
	icarusReturnMetrics.WithLabelValues("metrics").Observe(float64(metrics))
	i.serve.Next().Write(useBuffer.String())
	i.serve = i.serve.Next()
}

// aggPromDefaults gets everything out of the prometheus
// default registry and preps it for sending.
func aggPromDefaults(useBuffer *bytes.Buffer) {
	mfs, _ := prometheus.DefaultGatherer.Gather()
	useBuffer.Write([]byte("# Prometheus default registry metrics\n"))
	for _, mf := range mfs {
		expfmt.MetricFamilyToText(useBuffer, mf)
	}
}

//HandleFunc is an http handlefunc function. Apes a prometheus endpoint.
func (i *Icarus) HandleFunc(w http.ResponseWriter, r *http.Request) {
	useBuffer := bytes.NewBufferString("")
	aggPromDefaults(useBuffer)
	output := useBuffer.String() + i.serve.Read()
	icarusRequestCounter.Inc()
	icarusReturnSize.Observe(float64(len(output)))
	fmt.Fprint(w, output)
}