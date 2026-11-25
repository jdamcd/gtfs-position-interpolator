//go:build eval

// Replays captured feeds through the tracker twice: as captured (vehicle
// entities and trip updates) and with vehicle entities removed (trip updates
// alone). Run with:
//
//	go test -tags eval -run TestEval -v ./internal/track -args \
//	  -captures dir [-static feed.zip] [-merge] [-out dir] [-from HH:MM] [-to HH:MM]
package track

import (
	"archive/zip"
	"bytes"
	"cmp"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/geo"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static"
)

var (
	evalStatic   = flag.String("static", "", "static GTFS zip, by default static.zip in the capture directory")
	evalCaptures = flag.String("captures", "", "capture directory, one subdirectory of <unix time>.pb.gz per feed")
	evalMerge    = flag.Bool("merge", false, "the captures are split into tu and vp subdirectories, to be merged into one feed")
	evalOut      = flag.String("out", "", "directory for per-event CSVs")
	evalWarmup   = flag.Duration("warmup", 5*time.Minute, "time replayed before scoring starts, from the capture's start or before -from")
	evalFrom     = flag.String("from", "", "score from this local HH:MM, its first time in the capture")
	evalTo       = flag.String("to", "", "end the replay at this local HH:MM, its first time after -from")
	evalDebug    = flag.Float64("debug", 0, "print path A fixes further than this off the drawn leg")
)

const (
	pathA = "A vehicles+updates"
	pathB = "B updates only"
)

var paths = []string{pathA, pathB}

// Faster than subway trains run between stops, in metres per second.
const sprintSpeed = 25.0

type input struct {
	fetched   time.Time
	feed      string
	msg       *gtfs.FeedMessage
	tuChanged bool
	// Positions stripped from msg.
	fixes []fix
}

type stopKey struct{ trip, stop string }

type arrival struct {
	at           time.Time
	group, route string
}

type fix struct {
	trip  string
	at    time.Time
	point geo.Point
	group string
}

func readCapture(path string) (*gtfs.FeedMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	msg := &gtfs.FeedMessage{}
	return msg, proto.Unmarshal(body, msg)
}

type captureFile struct {
	path, feed string
	fetched    time.Time
}

// listCaptures sorts the files by fetch time, keeping ties in pattern then
// feed order so merge sees tu before vp. A file's feed is its directory.
func listCaptures(patterns ...string) ([]captureFile, error) {
	var files []captureFile
	for _, pattern := range patterns {
		names, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, p := range names {
			secs, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(p), ".pb.gz"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			files = append(files, captureFile{p, filepath.Base(filepath.Dir(p)), time.Unix(secs, 0)})
		}
	}
	slices.SortStableFunc(files, func(a, b captureFile) int { return a.fetched.Compare(b.fetched) })
	return files, nil
}

// nextClock finds the start of the first hhmm minute, in t's zone, that
// hasn't ended by t: -from 14:00 is the start of a capture begun at 14:00:02.
func nextClock(hhmm string, t time.Time) (time.Time, error) {
	c, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an HH:MM time", hhmm)
	}
	y, m, d := t.Date()
	at := time.Date(y, m, d, c.Hour(), c.Minute(), 0, 0, t.Location())
	if at.Before(t.Truncate(time.Minute)) {
		at = at.AddDate(0, 0, 1)
	}
	return at, nil
}

// clip keeps the files, which are in fetch order, from lead before -from to
// -to, each resolved to its first time in the capture.
func clip(files []captureFile, lead time.Duration) ([]captureFile, error) {
	if (*evalFrom == "" && *evalTo == "") || len(files) == 0 {
		return files, nil
	}
	first, last := files[0].fetched, files[len(files)-1].fetched
	from, to := first, last
	var err error
	if *evalFrom != "" {
		if from, err = nextClock(*evalFrom, first); err != nil {
			return nil, err
		}
		if from.After(last) {
			return nil, fmt.Errorf("-from %s is %s, after the capture ends at %s", *evalFrom, from.Format("Mon 15:04"), last.Format("Mon 15:04"))
		}
	}
	if *evalTo != "" {
		if to, err = nextClock(*evalTo, from.Add(time.Minute)); err != nil {
			return nil, err
		}
	}
	from = from.Add(-lead)
	return slices.DeleteFunc(files, func(f captureFile) bool { return f.fetched.Before(from) || f.fetched.After(to) }), nil
}

type decoded struct {
	msgs []*gtfs.FeedMessage
	err  error
}

// decodeAll decodes in parallel, as decoding is most of the load time.
func decodeAll(files []captureFile) <-chan decoded {
	out := make(chan decoded, 1)
	go func() {
		msgs := make([]*gtfs.FeedMessage, len(files))
		errs := make([]error, len(files))
		var wg sync.WaitGroup
		limit := make(chan struct{}, runtime.GOMAXPROCS(0))
		for i, f := range files {
			wg.Go(func() {
				limit <- struct{}{}
				defer func() { <-limit }()
				msg, err := readCapture(f.path)
				if err != nil {
					errs[i] = fmt.Errorf("%s: %w", f.path, err)
					return
				}
				msgs[i] = msg
			})
		}
		wg.Wait()
		out <- decoded{msgs, errors.Join(errs...)}
	}()
	return out
}

// decode works a chunk at a time, decoding the next while each runs on this
// one: a day of captures decoded at once would need around 100GB.
func decode(files []captureFile, each func(captureFile, *gtfs.FeedMessage)) error {
	const chunk = 64
	take := func() []captureFile {
		c := files[:min(chunk, len(files))]
		files = files[len(c):]
		return c
	}
	cur := take()
	pending := decodeAll(cur)
	for len(cur) > 0 {
		d := <-pending
		if d.err != nil {
			return d.err
		}
		next := take()
		pending = decodeAll(next)
		for i, f := range cur {
			each(f, d.msgs[i])
		}
		cur = next
	}
	return nil
}

func column(zipped []byte, file, key, value string) (map[string]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, static.ReadTable(zr, file, []string{key, value}, func(row []string) error {
		out[row[0]] = row[1]
		return nil
	})
}

func routeTypes(zipped []byte) (map[string]string, error) {
	kinds, err := column(zipped, "routes.txt", "route_id", "route_type")
	if err != nil {
		return nil, err
	}
	names := map[string]string{"0": "light rail", "1": "subway", "2": "commuter rail"}
	for id, kind := range kinds {
		kinds[id] = names[kind]
	}
	return kinds, nil
}

func entityRoute(e *gtfs.FeedEntity) string {
	if v := e.GetVehicle(); v != nil {
		return v.GetTrip().GetRouteId()
	}
	return e.GetTripUpdate().GetTrip().GetRouteId()
}

func keepRoutes(msg *gtfs.FeedMessage, keep map[string]string) {
	var kept []*gtfs.FeedEntity
	for _, e := range msg.GetEntity() {
		if keep[entityRoute(e)] != "" {
			kept = append(kept, e)
		}
	}
	msg.Entity = kept
}

// strip removes the vehicle positions from msg, so neither path can read
// them, and returns them as fixes.
func strip(msg *gtfs.FeedMessage, feed string, groups map[string]string) []fix {
	var fixes []fix
	for _, e := range msg.GetEntity() {
		v := e.GetVehicle()
		if v == nil {
			continue
		}
		if p := v.GetPosition(); p != nil && v.GetTrip().GetTripId() != "" {
			fixes = append(fixes, fix{tripKey(v.GetTrip()), time.Unix(int64(v.GetTimestamp()), 0),
				geo.Point{Lat: float64(p.GetLatitude()), Lon: float64(p.GetLongitude())}, group(groups, feed, v.GetTrip())})
		}
		v.Position = nil
	}
	return fixes
}

// stream calls each with the messages as the poller would hand them to the
// tracker: one per fetch, or with merge, tu and vp combined once both arrive.
func stream(files []captureFile, merge bool, groups map[string]string, each func(input)) error {
	var tu, vp *gtfs.FeedMessage
	return decode(files, func(f captureFile, msg *gtfs.FeedMessage) {
		if !merge {
			each(input{f.fetched, f.feed, msg, true, strip(msg, f.feed, groups)})
			return
		}
		keepRoutes(msg, groups)
		var fixes []fix
		if f.feed == "tu" {
			tu = msg
		} else {
			vp, fixes = msg, strip(msg, "", groups)
		}
		if tu == nil || vp == nil {
			return
		}
		header := proto.Clone(tu.GetHeader()).(*gtfs.FeedHeader)
		header.Timestamp = proto.Uint64(max(tu.GetHeader().GetTimestamp(), vp.GetHeader().GetTimestamp()))
		merged := &gtfs.FeedMessage{Header: header, Entity: slices.Concat(tu.GetEntity(), vp.GetEntity())}
		each(input{f.fetched, "merged", merged, f.feed == "tu", fixes})
	})
}

func group(groups map[string]string, feed string, trip *gtfs.TripDescriptor) string {
	if groups != nil {
		return groups[trip.GetRouteId()]
	}
	return feed
}

// references takes the first time each train's vehicle entity reports it
// STOPPED_AT each stop, from the vehicle's own timestamp. A stop the train is
// already at when first seen is left out, as it arrived before the capture
// began.
type references struct {
	data     *static.Data
	groups   map[string]string
	firstAt  map[string]string
	arrivals map[stopKey]arrival
}

func (r *references) add(in input) {
	for _, e := range in.msg.GetEntity() {
		v := e.GetVehicle()
		if v == nil || v.GetTrip().GetTripId() == "" {
			continue
		}
		key := tripKey(v.GetTrip())
		stopped := v.GetCurrentStatus() == gtfs.VehiclePosition_STOPPED_AT
		if _, seen := r.firstAt[key]; !seen {
			r.firstAt[key] = ""
			if stopped {
				r.firstAt[key] = v.GetStopId()
			}
		}
		if !stopped || !r.data.HasStop(v.GetStopId()) || r.firstAt[key] == v.GetStopId() {
			continue
		}
		k := stopKey{key, v.GetStopId()}
		if _, ok := r.arrivals[k]; ok {
			continue
		}
		at := in.fetched
		if ts := v.GetTimestamp(); ts != 0 {
			at = time.Unix(int64(ts), 0)
		}
		route := cmp.Or(v.GetTrip().GetRouteId(), r.data.Route(v.GetTrip().GetTripId()))
		r.arrivals[k] = arrival{at, group(r.groups, in.feed, v.GetTrip()), route}
	}
}

func forPath(in input, path string) (*gtfs.FeedMessage, bool) {
	if path == pathA {
		return in.msg, true
	}
	if !in.tuChanged {
		return nil, false
	}
	out := &gtfs.FeedMessage{Header: in.msg.GetHeader()}
	for _, e := range in.msg.GetEntity() {
		if e.GetVehicle() == nil {
			out.Entity = append(out.Entity, e)
		}
	}
	return out, true
}

type result struct {
	firstDrawn map[stopKey]time.Time
	// jumped marks stops the drawn train passed without being drawn there:
	// it was moved on to the next hop from short of the stop.
	jumped map[stopKey]bool
	errors map[int]float64
	// For fixes taken while the train is drawn moving: how far along the leg
	// the fix is ahead of the drawn train (+ means drawn behind), and how far
	// the fix is off the leg's line.
	along, away map[int]float64
	tallies     map[slot]*tally
	events      []event
}

// A slot is the trains of one group and route drawn in one hour.
type slot struct {
	group, route string
	hour         int64
}

func hourOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, t.Hour(), 0, 0, 0, t.Location())
}

// An event is a one-second step that jumped or went backwards.
type event struct {
	slot
	kind     string
	returned bool
	trip     string
	at       time.Time
	from, to placement
	metres   float64
}

// A tally is what a set of trains did while scored.
type tally struct {
	// Train-seconds drawn, and those drawn faster than sprintSpeed.
	seconds, sprints int
	// Distances of one-second steps faster than 40 m/s.
	leaps []float64
	// One-second steps against the direction of travel, and those of them
	// that put the train back at the stop it had left.
	backSteps, returned int
}

func (t *tally) count(e event) {
	if e.kind == "jump" {
		t.leaps = append(t.leaps, e.metres)
		return
	}
	t.backSteps++
	if e.returned {
		t.returned++
	}
}

func (r result) total(match func(slot) bool) tally {
	var sum tally
	for s, t := range r.tallies {
		if match(s) {
			sum.seconds += t.seconds
			sum.sprints += t.sprints
		}
	}
	for _, e := range r.events {
		if match(e.slot) {
			sum.count(e)
		}
	}
	slices.Sort(sum.leaps)
	return sum
}

func inGroup(g string) func(slot) bool {
	return func(s slot) bool { return g == "all" || s.group == g }
}

type drawnState struct {
	at       placement
	obs      observation
	leg      *leg
	distance float64
	speed    float64
	moving   bool
}

func stateOf(tr *train, now time.Time, data *static.Data) drawnState {
	s := drawnState{at: tr.place(now, data), obs: tr.obs, leg: tr.leg}
	if s.leg != nil {
		s.distance, s.speed = s.leg.advance(now)
		s.moving = s.distance < s.leg.line.Length()
	}
	return s
}

func angleBetween(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}

// backwards says whether a step from prev to cur runs against the direction
// of travel. Only states on a leg vote: a stop's own bearing is a fallback
// that faces the wrong way for some routes.
func backwards(prev, cur drawnState) bool {
	dir := geo.Bearing(prev.at.point, cur.at.point)
	votes := 0
	for _, s := range []drawnState{prev, cur} {
		if s.leg == nil || !s.at.hasBearing {
			continue
		}
		if angleBetween(dir, s.at.bearing) < 120 {
			return false
		}
		votes++
	}
	return votes > 0
}

func replay(data *static.Data, inputs <-chan input, path string, fixesAt map[int64][]int, fixes []fix, groups map[string]string, scoreFrom time.Time) result {
	config := Config{MaxAge: 10 * time.Minute, Ramp: 15 * time.Second}
	tracker := New(data, config)
	res := result{
		firstDrawn: map[stopKey]time.Time{},
		jumped:     map[stopKey]bool{},
		errors:     map[int]float64{},
		along:      map[int]float64{},
		away:       map[int]float64{},
		tallies:    map[slot]*tally{},
	}
	lastTarget := map[string]string{}
	lastStood := map[string]string{}
	states, lastState := map[string]drawnState{}, map[string]drawnState{}
	next, more := <-inputs
	for now := next.fetched; more; now = now.Add(time.Second) {
		for ; more && !next.fetched.After(now); next, more = <-inputs {
			if msg, ok := forPath(next, path); ok {
				tracker.Apply(next.feed, msg, next.fetched)
			}
		}
		hour := hourOf(now).Unix()
		lastState, states = states, lastState
		clear(states)
		for feed, snap := range tracker.feeds {
			for key, tr := range snap.trains {
				if now.Sub(tr.timestamp) > config.MaxAge {
					continue
				}
				cur := stateOf(tr, now, data)
				states[key] = cur
				if !now.Before(scoreFrom) {
					k := slot{group(groups, feed, tr.trip), tr.trip.GetRouteId(), hour}
					t := res.tallies[k]
					if t == nil {
						t = &tally{}
						res.tallies[k] = t
					}
					t.seconds++
					if cur.speed > sprintSpeed {
						t.sprints++
					}
					if prev, ok := lastState[key]; ok {
						d := geo.Distance(prev.at.point, cur.at.point)
						if d > 40 {
							res.events = append(res.events, event{k, "jump", false, key, now, prev.at, cur.at, d})
						}
						if d > 10 && backwards(prev, cur) {
							returned := prev.moving && !cur.moving && (cur.at.stop == prev.obs.from || cur.at.stop == lastStood[key])
							res.events = append(res.events, event{k, "back", returned, key, now, prev.at, cur.at, d})
						}
					}
				}
				if cur.at.status == gtfs.VehiclePosition_STOPPED_AT {
					lastStood[key] = cur.at.stop
				}
				target := tr.obs.to
				switch {
				case tr.leg != nil:
					target = tr.leg.to
				case tr.obs.at != "":
					target = tr.obs.at
				}
				if last := lastTarget[key]; last != "" && last != target {
					k := stopKey{key, last}
					if _, ok := res.firstDrawn[k]; !ok {
						res.firstDrawn[k], res.jumped[k] = now, true
					}
				}
				lastTarget[key] = target
				// Drawn at its target once the leg has run out, or straight away
				// without one.
				if target != "" && (cur.leg == nil || cur.distance >= cur.leg.line.Length()-0.5) {
					k := stopKey{key, target}
					if _, ok := res.firstDrawn[k]; !ok {
						res.firstDrawn[k] = now
					}
				}
			}
		}
		for _, fi := range fixesAt[now.Unix()] {
			f := fixes[fi]
			s, ok := states[f.trip]
			if !ok {
				continue
			}
			res.errors[fi] = geo.Distance(s.at.point, f.point)
			if !s.moving {
				continue
			}
			l := s.leg
			along, away := l.line.Project(f.point, 0, l.line.Length())
			res.along[fi], res.away[fi] = along-s.distance, away
			if path == pathA && *evalDebug > 0 && away > *evalDebug {
				fmt.Printf("debug %s %s %s obs{at:%q from:%q to:%q eta:%s} leg.to=%s d=%.0f/%.0f off=%.0f\n",
					now.Format("15:04:05"), f.group, f.trip, s.obs.at, s.obs.from, s.obs.to, s.obs.eta.Format("15:04:05"),
					l.to, s.distance, l.line.Length(), away)
			}
		}
	}
	return res
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

func share(values []float64, pred func(float64) bool) float64 {
	n := 0
	for _, v := range values {
		if pred(v) {
			n++
		}
	}
	return pct(n, len(values))
}

func pct(n, of int) float64 {
	return 100 * float64(n) / float64(max(1, of))
}

func withAll[V any](groups map[string]V) []string {
	return append(slices.Sorted(maps.Keys(groups)), "all")
}

func cell[K comparable](m map[K]float64, k K) string {
	if v, ok := m[k]; ok {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return ""
}

func early(lags []float64) float64 { return share(lags, func(x float64) bool { return x < -10 }) }

func late(lags []float64) float64 { return share(lags, func(x float64) bool { return x > 30 }) }

func perHour(n int, hours float64) float64 { return float64(n) / math.Max(hours, 1e-9) }

// top returns the n keys with the highest scores, ties in key order.
func top(keys iter.Seq[string], n int, score func(string) float64) []string {
	sorted := slices.SortedFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(score(b), score(a)), cmp.Compare(a, b)) })
	return sorted[:min(n, len(sorted))]
}

func meanAbs(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += math.Abs(v)
	}
	return sum / float64(max(1, len(values)))
}

type lagRow struct {
	key stopKey
	slot
	ref time.Time
	lag map[string]float64
}

func groupBy[K comparable](rows []lagRow, key func(lagRow) K) map[K][]lagRow {
	out := map[K][]lagRow{}
	for _, r := range rows {
		out[key(r)] = append(out[key(r)], r)
	}
	return out
}

// lagStats counts the arrivals each path reached, and sorts the lags of those
// both reached, so the paths are compared like for like.
type lagStats struct {
	refs            int
	covered, jumped map[string]int
	lags            map[string][]float64
}

func summarize(rows []lagRow, results map[string]result) lagStats {
	s := lagStats{len(rows), map[string]int{}, map[string]int{}, map[string][]float64{}}
	for _, r := range rows {
		for _, p := range paths {
			if _, ok := r.lag[p]; ok {
				s.covered[p]++
				if results[p].jumped[r.key] {
					s.jumped[p]++
				}
			}
		}
		if len(r.lag) == len(paths) {
			for _, p := range paths {
				s.lags[p] = append(s.lags[p], r.lag[p])
			}
		}
	}
	for _, v := range s.lags {
		slices.Sort(v)
	}
	return s
}

func breakdown[K cmp.Ordered](title, heading string, lags []lagRow, results map[string]result, key func(slot) K, label func(K) string) {
	rows := groupBy(lags, func(r lagRow) K { return key(r.slot) })
	keys := map[K]bool{}
	for k := range rows {
		keys[k] = true
	}
	for _, p := range paths {
		for s := range results[p].tallies {
			keys[key(s)] = true
		}
	}
	fmt.Println(title)
	fmt.Printf("%-10s %-19s %6s %8s %7s %7s %8s %8s %8s %8s %8s %8s\n", heading, "path", "refs", "covered", "jumped", "median", ">10s er", ">30s lt", "train-h", "jumps/h", "back/h", "sprints")
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		s := summarize(rows[k], results)
		for _, p := range paths {
			sum := results[p].total(func(s slot) bool { return key(s) == k })
			hours := float64(sum.seconds) / 3600
			v := s.lags[p]
			fmt.Printf("%-10s %-19s %6d %7.0f%% %6.0f%% %7.0f %7.1f%% %7.1f%% %8.1f %8.2f %8.2f %7.1f%%\n", label(k), p, s.refs,
				pct(s.covered[p], s.refs), pct(s.jumped[p], s.covered[p]), quantile(v, 0.5), early(v), late(v),
				hours, perHour(len(sum.leaps), hours), perHour(sum.backSteps, hours), pct(sum.sprints, sum.seconds))
		}
	}
}

func routeList(routes map[string]bool) string {
	return strings.Join(slices.Sorted(maps.Keys(routes)), " ")
}

func TestEval(t *testing.T) {
	if *evalCaptures == "" {
		t.Skip("needs -captures")
	}
	staticPath := *evalStatic
	if staticPath == "" {
		staticPath = filepath.Join(*evalCaptures, "static.zip")
	}
	zipped, err := os.ReadFile(staticPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := static.Parse(zipped)
	if err != nil {
		t.Fatal(err)
	}
	var groups map[string]string
	if *evalMerge {
		if groups, err = routeTypes(zipped); err != nil {
			t.Fatal(err)
		}
	}
	patterns := []string{filepath.Join(*evalCaptures, "*", "*.pb.gz")}
	if *evalMerge {
		patterns = []string{filepath.Join(*evalCaptures, "tu", "*.pb.gz"), filepath.Join(*evalCaptures, "vp", "*.pb.gz")}
	}
	files, err := listCaptures(patterns...)
	if err != nil {
		t.Fatal(err)
	}
	if files, err = clip(files, *evalWarmup); err != nil {
		t.Fatal(err)
	}
	stopNames, err := column(zipped, "stops.txt", "stop_id", "stop_name")
	if err != nil {
		t.Fatal(err)
	}

	// A first pass collects the GPS fixes, as each turns up in a fetch after the
	// timestamp the replay scores it at. The second pass replays.
	refs := references{data, groups, map[string]string{}, map[stopKey]arrival{}}
	var fixes []fix
	type fixID struct {
		trip string
		at   int64
	}
	seenFix := map[fixID]bool{}
	var start, end time.Time
	snapshots := 0
	err = stream(files, *evalMerge, groups, func(in input) {
		if snapshots == 0 {
			start = in.fetched
		}
		end = in.fetched
		snapshots++
		for _, f := range in.fixes {
			if id := (fixID{f.trip, f.at.Unix()}); !seenFix[id] {
				seenFix[id] = true
				fixes = append(fixes, f)
			}
		}
		refs.add(in)
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshots == 0 {
		t.Fatal("no captures")
	}
	scoreFrom := start.Add(*evalWarmup)
	fmt.Printf("window %s – %s (%s), %d snapshots, scored from %s\n",
		start.Format("Mon 15:04:05"), end.Format("Mon 15:04:05 MST"), end.Sub(start).Round(time.Second), snapshots, scoreFrom.Format("15:04:05"))

	fixesAt := map[int64][]int{}
	var scored []int
	for i, f := range fixes {
		if !f.at.Before(scoreFrom) && !f.at.After(end) {
			fixesAt[f.at.Unix()] = append(fixesAt[f.at.Unix()], i)
			scored = append(scored, i)
		}
	}
	results := map[string]result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	inputs := make([]chan input, len(paths))
	for i, p := range paths {
		inputs[i] = make(chan input, 64)
		wg.Go(func() {
			r := replay(data, inputs[i], p, fixesAt, fixes, groups, scoreFrom)
			mu.Lock()
			results[p] = r
			mu.Unlock()
		})
	}
	err = stream(files, *evalMerge, groups, func(in input) {
		for _, ch := range inputs {
			ch <- in
		}
	})
	for _, ch := range inputs {
		close(ch)
	}
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}

	var lags []lagRow
	for k, a := range refs.arrivals {
		if a.at.Before(scoreFrom) || a.at.After(end.Add(-2*time.Minute)) {
			continue
		}
		row := lagRow{k, slot{a.group, a.route, hourOf(a.at).Unix()}, a.at, map[string]float64{}}
		for _, p := range paths {
			if at, ok := results[p].firstDrawn[k]; ok {
				row.lag[p] = at.Sub(a.at).Seconds()
			}
		}
		lags = append(lags, row)
	}
	slices.SortFunc(lags, func(a, b lagRow) int {
		return cmp.Or(a.ref.Compare(b.ref), cmp.Compare(a.key.trip, b.key.trip), cmp.Compare(a.key.stop, b.key.stop))
	})

	byGroup := groupBy(lags, func(r lagRow) string { return r.group })
	groupNames := withAll(byGroup)

	fmt.Println("\nARRIVAL LAG: drawn arrival minus the vehicle's first STOPPED_AT timestamp (seconds, + is late)")
	fmt.Println("Covered counts arrivals each path reached, jumped those it reached by moving the train past the stop; the lag columns use only arrivals both reached.")
	fmt.Printf("%-14s %-19s %6s %8s %7s %7s %7s %7s %7s %8s %8s\n", "group", "path", "refs", "covered", "jumped", "median", "mean|x|", "p10", "p90", ">10s er", ">30s lt")
	for _, g := range groupNames {
		rows := lags
		if g != "all" {
			rows = byGroup[g]
		}
		s := summarize(rows, results)
		for _, p := range paths {
			v := s.lags[p]
			fmt.Printf("%-14s %-19s %6d %7.0f%% %6.0f%% %7.0f %7.0f %7.0f %7.0f %7.1f%% %7.1f%%\n", g, p, s.refs,
				pct(s.covered[p], s.refs), pct(s.jumped[p], s.covered[p]), quantile(v, 0.5), meanAbs(v), quantile(v, 0.1), quantile(v, 0.9), early(v), late(v))
		}
	}

	fmt.Println("\nJUMPS: one-second steps where a drawn train moved more than 40m")
	fmt.Printf("%-14s %-19s %8s %7s %10s %9s %9s\n", "group", "path", "train-h", "jumps", "per tr-h", "median m", "max m")
	for _, g := range groupNames {
		for _, p := range paths {
			sum := results[p].total(inGroup(g))
			hours := float64(sum.seconds) / 3600
			fmt.Printf("%-14s %-19s %8.1f %7d %10.1f %9.0f %9.0f\n", g, p, hours, len(sum.leaps), perHour(len(sum.leaps), hours), quantile(sum.leaps, 0.5), quantile(sum.leaps, 1))
		}
	}

	fmt.Println("\nBACKWARDS: one-second steps of more than 10m against the direction of travel; returned counts those that put the train back at the stop it had left")
	fmt.Println("SPRINTS: share of train time drawn faster than 25 m/s")
	fmt.Printf("%-14s %-19s %8s %7s %10s %9s %8s\n", "group", "path", "train-h", "back", "per tr-h", "returned", "sprints")
	for _, g := range groupNames {
		for _, p := range paths {
			sum := results[p].total(inGroup(g))
			hours := float64(sum.seconds) / 3600
			fmt.Printf("%-14s %-19s %8.1f %7d %10.2f %9d %7.1f%%\n", g, p, hours, sum.backSteps, perHour(sum.backSteps, hours), sum.returned, pct(sum.sprints, sum.seconds))
		}
	}

	if len(fixesAt) > 0 {
		fmt.Println("\nPOSITION ERROR: distance from the drawn position to the GPS fix at the fix's timestamp (metres)")
		fmt.Println("Coverage counts fixes each path drew the train for; the error columns use only fixes both drew.")
		fmt.Printf("%-14s %-19s %6s %8s %7s %7s %7s %7s %7s\n", "group", "path", "fixes", "covered", "median", "mean", "p90", "<100m", "<250m")
		fixGroups := map[string]bool{}
		for _, i := range scored {
			fixGroups[fixes[i].group] = true
		}
		for _, g := range withAll(fixGroups) {
			both := map[string][]float64{}
			covered := map[string]int{}
			n := 0
			for _, i := range scored {
				if g != "all" && fixes[i].group != g {
					continue
				}
				n++
				ok := true
				for _, p := range paths {
					if _, has := results[p].errors[i]; has {
						covered[p]++
					} else {
						ok = false
					}
				}
				if ok {
					for _, p := range paths {
						both[p] = append(both[p], results[p].errors[i])
					}
				}
			}
			for _, p := range paths {
				v := both[p]
				slices.Sort(v)
				fmt.Printf("%-14s %-19s %6d %7.0f%% %7.0f %7.0f %7.0f %6.1f%% %6.1f%%\n", g, p, n,
					pct(covered[p], n), quantile(v, 0.5), meanAbs(v), quantile(v, 0.9),
					share(v, func(x float64) bool { return x < 100 }), share(v, func(x float64) bool { return x < 250 }))
			}
		}

		fmt.Println("\nMOVING: fixes taken while both paths draw the train between stops (metres)")
		fmt.Println("Along is how far the GPS fix is ahead of the drawn train along its leg (+ means drawn behind); off is the fix's distance from the leg's line.")
		fmt.Printf("%-14s %-19s %6s %7s %7s %7s %7s %8s %8s\n", "group", "path", "fixes", "along", "|along|", "p10", "p90", "off med", "off p90")
		allGroups := map[string]bool{}
		for _, f := range fixes {
			allGroups[f.group] = true
		}
		for _, g := range withAll(allGroups) {
			alongs, aways := map[string][]float64{}, map[string][]float64{}
			for _, i := range scored {
				if g != "all" && fixes[i].group != g {
					continue
				}
				_, okA := results[pathA].along[i]
				_, okB := results[pathB].along[i]
				if !okA || !okB {
					continue
				}
				for _, p := range paths {
					alongs[p] = append(alongs[p], results[p].along[i])
					aways[p] = append(aways[p], results[p].away[i])
				}
			}
			for _, p := range paths {
				a, o := alongs[p], aways[p]
				slices.Sort(a)
				slices.Sort(o)
				fmt.Printf("%-14s %-19s %6d %7.0f %7.0f %7.0f %7.0f %8.0f %8.0f\n", g, p, len(a),
					quantile(a, 0.5), meanAbs(a), quantile(a, 0.1), quantile(a, 0.9), quantile(o, 0.5), quantile(o, 0.9))
			}
		}
	}

	breakdown("\nBY HOUR: arrivals by the hour of their reference time, train time by the hour it was drawn", "hour", lags, results,
		func(s slot) int64 { return s.hour }, func(h int64) string { return time.Unix(h, 0).Format("Mon 15:04") })
	breakdown("\nBY ROUTE", "route", lags, results,
		func(s slot) string { return s.route }, func(r string) string { return r })

	const listed, minArrivals = 10, 30
	fmt.Printf("\nSTOPS FURTHEST OFF: each path's highest mean |lag|, among stops with at least %d arrivals both paths reached\n", minArrivals)
	fmt.Printf("%-19s %-8s %-24s %-14s %6s %7s %7s %8s %8s\n", "path", "stop", "name", "routes", "refs", "median", "mean|x|", ">10s er", ">30s lt")
	byStop := groupBy(lags, func(r lagRow) string { return r.key.stop })
	stats := map[string]lagStats{}
	stopRoutes := map[string]map[string]bool{}
	var busy []string
	for stop, rows := range byStop {
		stats[stop] = summarize(rows, results)
		if len(stats[stop].lags[pathA]) >= minArrivals {
			busy = append(busy, stop)
		}
		stopRoutes[stop] = map[string]bool{}
		for _, r := range rows {
			stopRoutes[stop][r.route] = true
		}
	}
	for _, p := range paths {
		for _, stop := range top(slices.Values(busy), listed, func(s string) float64 { return meanAbs(stats[s].lags[p]) }) {
			v := stats[stop].lags[p]
			fmt.Printf("%-19s %-8s %-24.24s %-14.14s %6d %7.0f %7.0f %7.1f%% %7.1f%%\n", p, stop, stopNames[stop], routeList(stopRoutes[stop]), len(v),
				quantile(v, 0.5), meanAbs(v), early(v), late(v))
		}
	}

	fmt.Println("\nWHERE TRAINS JUMP OR STEP BACK: each path's stops with the most jumps and backward steps by trains drawn at or heading for them")
	fmt.Printf("%-19s %-8s %-24s %-14s %7s %7s %9s %9s\n", "path", "stop", "name", "routes", "jumps", "back", "returned", "median m")
	type place struct {
		tally
		routes map[string]bool
	}
	for _, p := range paths {
		places := map[string]*place{}
		for _, e := range results[p].events {
			pl := places[e.from.stop]
			if pl == nil {
				pl = &place{routes: map[string]bool{}}
				places[e.from.stop] = pl
			}
			pl.count(e)
			pl.routes[e.route] = true
		}
		for _, stop := range top(maps.Keys(places), listed, func(s string) float64 { return float64(len(places[s].leaps) + places[s].backSteps) }) {
			pl := places[stop]
			slices.Sort(pl.leaps)
			fmt.Printf("%-19s %-8s %-24.24s %-14.14s %7d %7d %9d %9.0f\n", p, stop, stopNames[stop], routeList(pl.routes), len(pl.leaps), pl.backSteps, pl.returned, quantile(pl.leaps, 0.5))
		}
	}

	if *evalOut != "" {
		if err := os.MkdirAll(*evalOut, 0o755); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString("trip,stop,group,ref_unix,lag_a,lag_b,route\n")
		for _, r := range lags {
			fmt.Fprintf(&b, "%s,%s,%s,%d,%s,%s,%s\n", r.key.trip, r.key.stop, r.group, r.ref.Unix(), cell(r.lag, pathA), cell(r.lag, pathB), r.route)
		}
		if err := os.WriteFile(filepath.Join(*evalOut, "arrivals.csv"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		b.Reset()
		b.WriteString("path,kind,returned,trip,route,unix,from_stop,to_stop,from_lat,from_lon,lat,lon,metres\n")
		for _, p := range paths {
			events := results[p].events
			slices.SortFunc(events, func(a, b event) int {
				return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.trip, b.trip), cmp.Compare(a.kind, b.kind))
			})
			for _, e := range events {
				fmt.Fprintf(&b, "%s,%s,%t,%s,%s,%d,%s,%s,%.6f,%.6f,%.6f,%.6f,%.0f\n", p[:1], e.kind, e.returned, e.trip, e.route, e.at.Unix(),
					e.from.stop, e.to.stop, e.from.point.Lat, e.from.point.Lon, e.to.point.Lat, e.to.point.Lon, e.metres)
			}
		}
		if err := os.WriteFile(filepath.Join(*evalOut, "events.csv"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		if len(fixesAt) > 0 {
			b.Reset()
			b.WriteString("trip,group,unix,lat,lon,err_a,err_b\n")
			for _, i := range scored {
				f := fixes[i]
				fmt.Fprintf(&b, "%s,%s,%d,%.6f,%.6f,%s,%s\n", f.trip, f.group, f.at.Unix(), f.point.Lat, f.point.Lon, cell(results[pathA].errors, i), cell(results[pathB].errors, i))
			}
			if err := os.WriteFile(filepath.Join(*evalOut, "fixes.csv"), []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

var evalTrace = flag.String("trace", "", "trip ID substring to print the raw feed history of")

func TestTrace(t *testing.T) {
	if *evalTrace == "" || *evalCaptures == "" {
		t.Skip("needs -trace and -captures")
	}
	files, err := listCaptures(filepath.Join(*evalCaptures, "*.pb.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if files, err = clip(files, 0); err != nil {
		t.Fatal(err)
	}
	err = decode(files, func(f captureFile, msg *gtfs.FeedMessage) {
		for _, e := range msg.GetEntity() {
			if v := e.GetVehicle(); v != nil && strings.Contains(v.GetTrip().GetTripId(), *evalTrace) {
				fmt.Printf("%s hdr %s  VEH %-13s %-5s ts %s\n", f.fetched.Format("15:04:05"), time.Unix(int64(msg.GetHeader().GetTimestamp()), 0).Format("15:04:05"),
					v.GetCurrentStatus(), v.GetStopId(), time.Unix(int64(v.GetTimestamp()), 0).Format("15:04:05"))
			}
			if tu := e.GetTripUpdate(); tu != nil && strings.Contains(tu.GetTrip().GetTripId(), *evalTrace) {
				var b strings.Builder
				for i, u := range tu.GetStopTimeUpdate() {
					if i == 3 {
						break
					}
					fmt.Fprintf(&b, " %s arr %s dep %s |", u.GetStopId(), time.Unix(u.GetArrival().GetTime(), 0).Format("15:04:05"), time.Unix(u.GetDeparture().GetTime(), 0).Format("15:04:05"))
				}
				fmt.Printf("%s                TU %s\n", f.fetched.Format("15:04:05"), b.String())
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
