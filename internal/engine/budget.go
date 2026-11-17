package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// The settings that bound what a task uses since a person last acted, and
// what the instance spends in a day.
const (
	keyTaskTimeout = "task_timeout_minutes"
	keyTaskTurns   = "task_max_turns"
	keyTaskBudget  = "task_budget_usd"
	keyDayBudget   = "daily_budget_usd"
	keyPrices      = "model_prices"
)

// capKeys are the settings a project may cap for its own tasks.
var capKeys = []string{keyTaskTimeout, keyTaskTurns, keyTaskBudget}

// defaultPrice is the model_prices key that prices any model without an entry of its own.
const defaultPrice = "*"

// Limits are a task's budget since the last human action, and the instance's
// money for the day. A money limit of 0 is no cap.
type Limits struct {
	Turns        int
	SessionTime  time.Duration
	TaskMicroUSD int64
	DayMicroUSD  int64
}

// capped reports whether a money cap is in force.
func (l Limits) capped() bool { return l.TaskMicroUSD > 0 || l.DayMicroUSD > 0 }

// Spent is the money the caps count, read only while one is in force.
type Spent struct {
	TaskMicroUSD int64    // by the task since the last human action
	DayMicroUSD  int64    // by every task in the server's local day
	Unpriced     []string // the models the task used since the last human action that have no price, sorted
}

// Price is a model's price in US dollars per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// readLimits reads a task's limits from the settings with its project's caps
// over them. Money is capped in key mode only.
func readLimits(settings, caps map[string]string) (Limits, error) {
	values := maps.Clone(settings)
	maps.Copy(values, caps)

	set := settingsReader{values: values}
	l := set.limits()

	if values["endpoint_mode"] != "key" {
		l.TaskMicroUSD, l.DayMicroUSD = 0, 0
	}

	return l, set.err
}

// Prices reads the model_prices setting; unset gives none.
func Prices(values map[string]string) (map[string]Price, error) {
	v := values[keyPrices]
	if v == "" {
		return nil, nil
	}

	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()

	var prices map[string]Price
	if err := dec.Decode(&prices); err != nil {
		return nil, fmt.Errorf("setting %s: want a JSON object of model ids, each with input, output, cache_read and cache_write in US dollars per million tokens: %w",
			keyPrices, err)
	}

	for model, p := range prices {
		if min(p.Input, p.Output, p.CacheRead, p.CacheWrite) < 0 {
			return nil, fmt.Errorf("setting %s: model %q has a negative price", keyPrices, model)
		}
	}

	return prices, nil
}

// Cost is what the proxy-counted rows of usage cost at prices, in micro-dollars,
// and the models among them without a price, sorted. Each bucket rounds on its
// own, as the proxy counts a response.
func Cost(usage []store.Usage, prices map[string]Price) (microUSD int64, unpriced []string) {
	for _, u := range usage {
		if u.Source != "proxy" {
			continue
		}

		p, ok := prices[u.Model]
		if !ok {
			p, ok = prices[defaultPrice]
		}

		if !ok {
			unpriced = append(unpriced, u.Model)

			continue
		}

		microUSD += micro(u.Input, p.Input) + micro(u.Output, p.Output) + micro(u.CacheRead, p.CacheRead) + micro(u.CacheWrite, p.CacheWrite)
	}

	slices.Sort(unpriced)

	return microUSD, slices.Compact(unpriced)
}

// micro is tokens at a price per million tokens, in whole micro-dollars.
func micro(tokens int64, perMillion float64) int64 {
	return int64(math.Round(float64(tokens) * perMillion))
}

// spent reads what the task spent since the person's action since and what
// every task spent in now's local day, at the prices it also returns.
func (l *Loop) spent(ctx context.Context, taskID, since int64, settings map[string]string, now time.Time) (Spent, map[string]Price, error) {
	prices, err := Prices(settings)
	if err != nil {
		return Spent{}, nil, err
	}

	task, err := l.Store.UsageSince(ctx, taskID, since)
	if err != nil {
		return Spent{}, nil, err
	}

	from, to := day(now)

	all, err := l.Store.UsageBetween(ctx, from, to)
	if err != nil {
		return Spent{}, nil, err
	}

	var s Spent

	s.TaskMicroUSD, s.Unpriced = Cost(task, prices)
	s.DayMicroUSD, _ = Cost(all, prices)

	return s, prices, nil
}

// day is the local calendar day t falls in, in t's own location: its midnight and the next.
func day(t time.Time) (start, end time.Time) {
	y, m, d := t.Date()

	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()), time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

// overBudget is the stop of a task that used its turns, its session time or
// its money since a person last acted, or that used a model without a price
// while a money cap is in force.
func overBudget(s Snapshot) (Action, bool) {
	switch {
	case s.Facts.Turns >= s.Limits.Turns:
		return halt(StopBudget, ClearRetry, "The task used its turns.", turnsUsed(s)), true
	case s.Facts.SessionTime >= s.Limits.SessionTime:
		return halt(StopTimeout, ClearRetry, "The task used its session time.", timeUsed(s)), true
	case len(s.Spent.Unpriced) > 0:
		return halt(StopBudget, ClearRetry, "The task used a model without a price.", line(fmt.Sprintf(
			"The task used %s, which model_prices does not price, so its spend cannot be counted against the money cap.",
			strings.Join(s.Spent.Unpriced, ", ")))), true
	case s.Limits.TaskMicroUSD > 0 && s.Spent.TaskMicroUSD >= s.Limits.TaskMicroUSD:
		return halt(StopBudget, ClearRetry, "The task spent its money.", fmt.Sprintf("The task spent %s of its %s since a person last acted.",
			dollars(s.Spent.TaskMicroUSD), dollars(s.Limits.TaskMicroUSD))), true
	}

	return Action{}, false
}

// dayUsed is the wait of a task while the instance has spent its daily cap.
func dayUsed(s Snapshot) (Action, bool) {
	if s.Limits.DayMicroUSD == 0 || s.Spent.DayMicroUSD < s.Limits.DayMicroUSD {
		return Action{}, false
	}

	_, next := day(s.Now)

	return Action{Kind: KindWait, Why: fmt.Sprintf("The instance spent %s of its daily %s; work resumes at local midnight.",
		dollars(s.Spent.DayMicroUSD), dollars(s.Limits.DayMicroUSD)), Until: next}, true
}

func turnsUsed(s Snapshot) string {
	return fmt.Sprintf("The task used %d of its %d turns since a person last acted.", s.Facts.Turns, s.Limits.Turns)
}

func timeUsed(s Snapshot) string {
	return fmt.Sprintf("The task's sessions ran %.0f of its %.0f minutes since a person last acted.",
		s.Facts.SessionTime.Minutes(), s.Limits.SessionTime.Minutes())
}

// dollars prints micro-dollars as dollars and cents.
func dollars(microUSD int64) string {
	return fmt.Sprintf("$%.2f", float64(microUSD)/1e6)
}

// left is what a session may use of the task's budget; overBudget keeps both above zero.
func left(s Snapshot) (turns int, d time.Duration) {
	return s.Limits.Turns - s.Facts.Turns, s.Limits.SessionTime - s.Facts.SessionTime
}

// moneyLeft is what a task under a money cap may still spend: the least that
// a cap in force leaves, never below 0.
func moneyLeft(s Snapshot) int64 {
	left := int64(math.MaxInt64)

	if c := s.Limits.TaskMicroUSD; c > 0 {
		left = c - s.Spent.TaskMicroUSD
	}

	if c := s.Limits.DayMicroUSD; c > 0 {
		left = min(left, c-s.Spent.DayMicroUSD)
	}

	return max(left, 0)
}
