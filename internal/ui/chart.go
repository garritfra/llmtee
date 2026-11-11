package ui

import (
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/garritfra/llmtee/internal/store"
)

const (
	hour = time.Hour
	day  = 24 * time.Hour
	week = 7 * day

	// maxSeries is the number of groups drawn in their own color before the
	// rest is merged into "Other".
	maxSeries = 5
	maxCols   = 400
	maxTicks  = 8
)

// splits are the dimensions the cost chart can be split by.
var splits = []struct{ Key, Label, Param string }{
	{"", "Total", ""},
	{store.ByModel, "Model", "model"},
	{store.ByKey, "Key", "key"},
	{store.ByUser, "User", "user"},
	{store.ByUpstream, "Upstream", "upstream"},
}

func splitFor(key string) (dim, param string) {
	for _, s := range splits {
		if s.Key == key {
			return s.Key, s.Param
		}
	}
	return "", ""
}

type segment struct {
	Name  string
	Color string
	Cost  float64
	Pct   float64 // share of the column
}

type column struct {
	Label   string // full period, for the tooltip and the data table
	Tick    string // x-axis label, empty for most columns
	Minor   bool   // tick hidden on narrow screens
	Edge    string // "start" or "end" for ticks near the chart edges
	Cost    float64
	Reqs    int64
	Height  float64 // percent of the plot height
	Partial bool    // the current, still running bucket
	Flip    bool    // tooltip opens to the left
	Segs    []segment
	Values  []float64 // cost per entry of chart.Series
}

type series struct {
	Name  string
	Color string
	Cost  float64
	Share float64
	URL   string // filtered request list, empty for "Other"
	Group string
	Other bool
}

type yTick struct {
	Label string
	Pos   float64 // percent from the bottom
}

type chart struct {
	Unit   string // "hour", "day" or "week"
	Cols   []column
	Series []series
	YTicks []yTick
	Split  bool
}

// seriesColor returns the CSS class of the i-th series.
func seriesColor(i int, other bool) string {
	if other {
		return "s-other"
	}
	return "s" + strconv.Itoa(i)
}

// chartKeep chooses the groups drawn as their own series from the top groups
// by cost. If at most maxSeries+1 groups exist, all of them are kept: an
// "Other" series with a single member would only hide its name.
func chartKeep(top []store.SpendRow) []string {
	n := len(top)
	if n > maxSeries+1 {
		n = maxSeries
	}
	keep := make([]string, n)
	for i := range keep {
		keep[i] = top[i].Group
	}
	return keep
}

// bucketStart returns the start of the bucket that contains t.
func bucketStart(t time.Time, unit time.Duration) time.Time {
	t = t.UTC()
	if unit == week {
		d := t.Truncate(day)
		return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7)) // Monday
	}
	return t.Truncate(unit)
}

// buildChart turns series rows into columns. start is the first bucket; rows
// must use unit-sized buckets, or day buckets when unit is week.
func buildChart(rows []store.SeriesRow, keep []string, start, now time.Time, unit time.Duration,
	name func(string) string, link func(string) string) chart {
	c := chart{Unit: map[time.Duration]string{hour: "hour", day: "day", week: "week"}[unit], Split: keep != nil}
	start = bucketStart(start, unit)
	end := bucketStart(now, unit)
	n := int(end.Sub(start)/unit) + 1
	if n > maxCols { // drop the oldest buckets, never the newest
		start = start.Add(time.Duration(n-maxCols) * unit)
		n = maxCols
	}
	if n < 1 {
		return c
	}

	// Series in stacking order: kept groups by cost, then "Other".
	all := []series{{Name: "Cost", Color: seriesColor(0, false)}}
	idx := map[string]int{}
	if c.Split {
		all = all[:0]
		for i, g := range keep {
			idx[g] = i
			all = append(all, series{Name: name(g), Color: seriesColor(i, false), Group: g, URL: link(g)})
		}
		all = append(all, series{Name: "Other", Color: seriesColor(0, true), Other: true})
	}
	c.Cols = make([]column, n)
	for i := range c.Cols {
		c.Cols[i].Values = make([]float64, len(all))
	}
	for _, r := range rows {
		ci := int(bucketStart(time.UnixMilli(r.Start), unit).Sub(start) / unit)
		if ci < 0 || ci >= n {
			continue
		}
		si := 0
		if c.Split {
			var ok bool
			if si, ok = idx[r.Group]; !ok || r.Other {
				si = len(all) - 1
			}
		}
		col := &c.Cols[ci]
		col.Values[si] += r.CostUSD
		col.Cost += r.CostUSD
		col.Reqs += r.Requests
		all[si].Cost += r.CostUSD
	}

	// Series without cost in the window add nothing to the legend.
	var used []int
	total := 0.0
	for si, s := range all {
		total += s.Cost
		if s.Cost > 0 || !c.Split {
			used = append(used, si)
		}
	}
	for _, si := range used {
		s := all[si]
		if total > 0 {
			s.Share = s.Cost / total * 100
		}
		c.Series = append(c.Series, s)
	}
	maxCost := 0.0
	for i := range c.Cols {
		col := &c.Cols[i]
		col.Label = periodLabel(start.Add(time.Duration(i)*unit), unit)
		col.Partial = i == n-1
		col.Flip = i >= n/2
		values := make([]float64, len(used))
		for j, si := range used {
			values[j] = col.Values[si]
			if v := col.Values[si]; v > 0 {
				col.Segs = append(col.Segs, segment{Name: all[si].Name, Color: all[si].Color, Cost: v, Pct: v / col.Cost * 100})
			}
		}
		col.Values = values
		maxCost = math.Max(maxCost, col.Cost)
	}

	top, ticks := yScale(maxCost)
	c.YTicks = ticks
	for i := range c.Cols {
		c.Cols[i].Height = c.Cols[i].Cost / top * 100
	}
	xTicks(c.Cols, start, unit)
	return c
}

func periodLabel(t time.Time, unit time.Duration) string {
	switch unit {
	case hour:
		return t.Format("Mon Jan 2, 15:04") + "–" + t.Add(hour).Format("15:04") + " UTC"
	case week:
		return "Week of " + t.Format("Mon Jan 2, 2006")
	}
	return t.Format("Mon Jan 2, 2006")
}

// xTicks labels a few columns at natural boundaries: every 6 hours, every
// day, every Monday, the 1st and 15th of a month, or every month.
func xTicks(cols []column, start time.Time, unit time.Duration) {
	var at []int
	for i := range cols {
		t := start.Add(time.Duration(i) * unit)
		var label string
		switch {
		case unit == hour && t.Hour() == 0:
			label = t.Format("Jan 2")
		case unit == hour && t.Hour()%6 == 0:
			label = t.Format("15:04")
		case unit == day && len(cols) <= 10,
			unit == day && len(cols) <= 45 && t.Weekday() == time.Monday,
			unit == day && len(cols) > 45 && (t.Day() == 1 || t.Day() == 15):
			label = t.Format("Jan 2")
		case unit == week && t.Day() <= 7:
			label = t.Format("Jan")
			if t.Month() == time.January || len(at) == 0 {
				label = t.Format("Jan 2006")
			}
		}
		if label != "" {
			cols[i].Tick = label
			at = append(at, i)
		}
	}
	step := (len(at) + maxTicks - 1) / maxTicks
	shown := (len(at) + step - 1) / max(step, 1)
	for j, i := range at {
		if j%step != 0 {
			cols[i].Tick = ""
			continue
		}
		// Every other label is hidden on narrow screens.
		cols[i].Minor = shown > 4 && (j/step)%2 == 1
		edge := len(cols) / 12
		switch {
		case i < edge:
			cols[i].Edge = "start"
		case i >= len(cols)-edge:
			cols[i].Edge = "end"
		}
	}
}

// yScale returns a round axis maximum >= max and evenly spaced ticks.
func yScale(max float64) (float64, []yTick) {
	if max <= 0 {
		return 1, []yTick{{"$0", 0}}
	}
	step := niceStep(max / 4)
	n := int(math.Ceil(max/step - 1e-9))
	top := step * float64(n)
	ticks := make([]yTick, 0, n+1)
	for i := 0; i <= n; i++ {
		v := step * float64(i)
		ticks = append(ticks, yTick{axisUSD(v, step), v / top * 100})
	}
	return top, ticks
}

func niceStep(raw float64) float64 {
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5} {
		if raw <= m*p*(1+1e-9) {
			return m * p
		}
	}
	return 10 * p
}

// axisUSD formats an axis value with as few decimals as the step needs.
func axisUSD(v, step float64) string {
	if v == 0 {
		return "$0"
	}
	d := 0
	for d < 6 && math.Abs(step*math.Pow(10, float64(d))-math.Round(step*math.Pow(10, float64(d)))) > 1e-6 {
		d++
	}
	return "$" + commas(strconv.FormatFloat(v, 'f', d, 64))
}

// delta compares the spend of the selected window with the window of the same
// length right before it.
type delta struct {
	Show  bool
	Arrow string // decorative, the text carries the meaning
	Text  string
	Prev  float64
	Since string // start of the previous window
}

func compare(cur, prev float64, prevStart time.Time) delta {
	d := delta{Show: true, Prev: prev, Since: prevStart.UTC().Format("Jan 2, 15:04 UTC")}
	p := 0.0
	if prev > 0 {
		p = (cur - prev) / prev * 100
	}
	switch {
	case prev == 0 && cur == 0, prev > 0 && math.Abs(p) < 0.5:
		d.Text = "same as prior period"
	case prev == 0:
		d.Arrow, d.Text = "▲", "no spend in prior period"
	case cur/prev >= 100:
		d.Arrow, d.Text = "▲", "over 100× prior period"
	case cur/prev >= 2:
		d.Arrow, d.Text = "▲", strconv.FormatFloat(cur/prev, 'f', 1, 64)+"× prior period"
	case p > 0:
		d.Arrow, d.Text = "▲", strconv.FormatFloat(p, 'f', 0, 64)+"% more than prior period"
	default:
		d.Arrow, d.Text = "▼", strconv.FormatFloat(-p, 'f', 0, 64)+"% less than prior period"
	}
	return d
}

// breakdown is one "By model/key/..." table.
type breakdown struct {
	Title    string
	Dim      string
	SplitURL string
	Active   bool // the chart is split by this dimension
	Rows     []breakdownRow
	Other    *breakdownRow
}

type breakdownRow struct {
	Name      string
	URL       string
	Color     string
	Cost      float64
	Share     float64
	Requests  int64
	TokensIn  int64
	TokensOut int64
	Unpriced  int64
	Unset     bool
}

func newBreakdown(title, dim string, rows []store.SpendRow, limit int, total store.SpendRow,
	name func(string) string, link func(string) string, colors map[string]string) breakdown {
	b := breakdown{Title: title, Dim: dim}
	rest := total
	for _, r := range rows {
		br := breakdownRow{Name: name(r.Group), Cost: r.CostUSD, Requests: r.Requests, TokensIn: r.TokensIn,
			TokensOut: r.TokensOut, Unpriced: r.Unpriced, Unset: r.Group == "", Color: colors[r.Group]}
		if r.Group != "" {
			br.URL = link(r.Group)
		}
		if total.CostUSD > 0 {
			br.Share = r.CostUSD / total.CostUSD * 100
		}
		b.Rows = append(b.Rows, br)
		rest.Requests -= r.Requests
		rest.CostUSD -= r.CostUSD
		rest.TokensIn -= r.TokensIn
		rest.TokensOut -= r.TokensOut
		rest.Unpriced -= r.Unpriced
	}
	if len(rows) >= limit && rest.Requests > 0 {
		o := breakdownRow{Name: "Other", Cost: math.Max(rest.CostUSD, 0), Requests: rest.Requests,
			TokensIn: rest.TokensIn, TokensOut: rest.TokensOut, Unpriced: rest.Unpriced}
		if total.CostUSD > 0 {
			o.Share = o.Cost / total.CostUSD * 100
		}
		b.Other = &o
	}
	return b
}

// overviewURL builds an overview link that keeps the other parameters.
func overviewURL(rng, split string) string {
	v := url.Values{}
	if rng != "" {
		v.Set("range", rng)
	}
	if split != "" {
		v.Set("split", split)
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}
