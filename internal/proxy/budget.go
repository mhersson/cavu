package proxy

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
)

// Price is a model's price in US dollars per million tokens.
type Price struct{ Input, Output, CacheRead, CacheWrite float64 }

// Budget is what a run may still spend at the model route.
type Budget struct {
	CapMicroUSD int64
	Prices      map[string]Price // by the model id a response reports
}

// Why a response's spend cannot be counted, in words a person can act on.
const (
	whyEncoded = "The model endpoint answered compressed, so the run's spend cannot be counted; use an endpoint that answers uncompressed."
	whyType    = "The model endpoint answered with neither JSON nor an event stream, so the run's spend cannot be counted; use an endpoint that answers in the Messages API format."
	whyLarge   = "A model response was too large for the proxy to read its usage, so the run's spend cannot be counted; run without a money cap."
	whyNoUsage = "A model response ended without usage, so the run's spend cannot be counted; use an endpoint that reports usage."
	whyNoPrice = "Model %q has no price, so the run's spend cannot be counted; add a price for that model."
)

const refusal = `{"type":"error","error":{"type":"billing_error","message":"This run's money cap stops its model requests."}}`

// ledger counts a run's spend at the model route. Requests admitted before it
// refuses all end and all count, so a run can pass its cap by what was in flight.
type ledger struct {
	Budget
	onBudget func(reason string)

	mu      sync.Mutex
	spent   int64
	refused bool
}

func (l *ledger) refusing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.refused
}

// count adds one response's cost, or with why set notes that its cost is
// unknown. Either can start the refusing, which then lasts.
func (l *ledger) count(u Usage, why string) {
	p, priced := l.Prices[u.Model]
	if why == "" && !priced {
		why = fmt.Sprintf(whyNoPrice, u.Model)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if why == "" {
		l.spent += micro(u.Input, p.Input) + micro(u.Output, p.Output) +
			micro(u.CacheRead, p.CacheRead) + micro(u.CacheWrite, p.CacheWrite)
	}

	if l.refused || (why == "" && l.spent < l.CapMicroUSD) {
		return
	}

	l.refused = true

	if l.onBudget != nil {
		l.onBudget(why)
	}
}

// micro is tokens at a price per million tokens, in whole micro-dollars.
func micro(tokens int64, perMillion float64) int64 {
	return int64(math.Round(float64(tokens) * perMillion))
}

func refuse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = io.WriteString(w, refusal)
}
