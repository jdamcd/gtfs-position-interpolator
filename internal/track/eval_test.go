//go:build eval

// Replays captured feeds through the tracker twice: as captured (vehicle
// entities and trip updates) and with vehicle entities removed (trip updates
// alone). Run with:
//
//	go test -tags eval -run TestEval -v ./internal/track -args \
//	  -captures dir [-static feed.zip] [-merge] [-out dir]
package track

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
	evalWarmup   = flag.Duration("warmup", 5*time.Minute, "time at the start of the capture left out of scoring")
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
}

type stopKey struct{ trip, stop string }

type arrival struct {
	at    time.Time
	group string
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

type captured struct {
	fetched time.Time
	msg     *gtfs.FeedMessage
}

func readCaptures(dir string) ([]captured, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.pb.gz"))
	if err != nil {
		return nil, err
	}
	// Decoding is most of the load time, so files are read in parallel.
	out := make([]captured, len(files))
	errs := make([]error, len(files))
	var wg sync.WaitGroup
	limit := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, p := range files {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			secs, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(p), ".pb.gz"), 10, 64)
			if err != nil {
				errs[i] = err
				return
			}
			msg, err := readCapture(p)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", p, err)
				return
			}
			out[i] = captured{time.Unix(secs, 0), msg}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].fetched.Before(out[j].fetched) })
	return out, nil
}

func routeTypes(zipped []byte) (map[string]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, err
	}
	f, err := zr.Open("routes.txt")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	id, kind := -1, -1
	for i, c := range rows[0] {
		switch strings.TrimPrefix(c, "\xef\xbb\xbf") {
		case "route_id":
			id = i
		case "route_type":
			kind = i
		}
	}
	names := map[string]string{"0": "light rail", "1": "subway", "2": "commuter rail"}
	out := map[string]string{}
	for _, r := range rows[1:] {
		out[r[id]] = names[r[kind]]
	}
	return out, nil
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

// loadStream reads the captures in fetch order. Vehicle positions are
// recorded as fixes and then removed, so neither path can read them.
func loadStream(dir string, merge bool, groups map[string]string) ([]input, []fix, error) {
	var fixes []fix
	type fixID struct {
		trip  string
		stamp uint64
	}
	seenFix := map[fixID]bool{}
	strip := func(msg *gtfs.FeedMessage, feed string) {
		for _, e := range msg.GetEntity() {
			v := e.GetVehicle()
			if v == nil {
				continue
			}
			if p := v.GetPosition(); p != nil && v.GetTrip().GetTripId() != "" {
				key := tripKey(v.GetTrip())
				id := fixID{key, v.GetTimestamp()}
				if !seenFix[id] {
					seenFix[id] = true
					fixes = append(fixes, fix{key, time.Unix(int64(v.GetTimestamp()), 0),
						geo.Point{Lat: float64(p.GetLatitude()), Lon: float64(p.GetLongitude())}, group(groups, feed, v.GetTrip())})
				}
			}
			v.Position = nil
		}
	}

	if !merge {
		subdirs, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil, err
		}
		var stream []input
		for _, d := range subdirs {
			if !d.IsDir() {
				continue
			}
			caps, err := readCaptures(filepath.Join(dir, d.Name()))
			if err != nil {
				return nil, nil, err
			}
			for _, c := range caps {
				strip(c.msg, d.Name())
				stream = append(stream, input{c.fetched, d.Name(), c.msg, true})
			}
		}
		sort.SliceStable(stream, func(i, j int) bool { return stream[i].fetched.Before(stream[j].fetched) })
		return stream, fixes, nil
	}

	tus, err := readCaptures(filepath.Join(dir, "tu"))
	if err != nil {
		return nil, nil, err
	}
	vps, err := readCaptures(filepath.Join(dir, "vp"))
	if err != nil {
		return nil, nil, err
	}
	for _, c := range tus {
		keepRoutes(c.msg, groups)
	}
	for _, c := range vps {
		keepRoutes(c.msg, groups)
		strip(c.msg, "")
	}
	var stream []input
	var tu, vp *gtfs.FeedMessage
	i, j := 0, 0
	for i < len(tus) || j < len(vps) {
		var at time.Time
		tuChanged := false
		if j == len(vps) || (i < len(tus) && !tus[i].fetched.After(vps[j].fetched)) {
			tu, at, tuChanged = tus[i].msg, tus[i].fetched, true
			i++
		} else {
			vp, at = vps[j].msg, vps[j].fetched
			j++
		}
		if tu == nil || vp == nil {
			continue
		}
		header := proto.Clone(tu.GetHeader()).(*gtfs.FeedHeader)
		header.Timestamp = proto.Uint64(max(tu.GetHeader().GetTimestamp(), vp.GetHeader().GetTimestamp()))
		merged := &gtfs.FeedMessage{Header: header, Entity: slices.Concat(tu.GetEntity(), vp.GetEntity())}
		stream = append(stream, input{at, "merged", merged, tuChanged})
	}
	return stream, fixes, nil
}

func group(groups map[string]string, feed string, trip *gtfs.TripDescriptor) string {
	if groups != nil {
		return groups[trip.GetRouteId()]
	}
	return feed
}

// referenceArrivals takes the first time each train's vehicle entity reports
// it STOPPED_AT each stop, from the vehicle's own timestamp. A stop the train
// is already at when first seen is left out, as it arrived before the
// capture began.
func referenceArrivals(stream []input, data *static.Data, groups map[string]string) map[stopKey]arrival {
	out := map[stopKey]arrival{}
	firstAt := map[string]string{}
	for _, in := range stream {
		for _, e := range in.msg.GetEntity() {
			v := e.GetVehicle()
			if v == nil || v.GetTrip().GetTripId() == "" {
				continue
			}
			key := tripKey(v.GetTrip())
			stopped := v.GetCurrentStatus() == gtfs.VehiclePosition_STOPPED_AT
			if _, seen := firstAt[key]; !seen {
				firstAt[key] = ""
				if stopped {
					firstAt[key] = v.GetStopId()
				}
			}
			if !stopped || !data.HasStop(v.GetStopId()) || firstAt[key] == v.GetStopId() {
				continue
			}
			k := stopKey{key, v.GetStopId()}
			if _, ok := out[k]; ok {
				continue
			}
			at := in.fetched
			if ts := v.GetTimestamp(); ts != 0 {
				at = time.Unix(int64(ts), 0)
			}
			out[k] = arrival{at, group(groups, in.feed, v.GetTrip())}
		}
	}
	return out
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
	groups      map[string]*tally
}

// A tally is what one group of trains did while scored.
type tally struct {
	// Train-seconds drawn, and those drawn faster than sprintSpeed.
	seconds, sprints int
	// Distances of one-second steps faster than 40 m/s.
	leaps []float64
	// One-second steps against the direction of travel, and those of them
	// that put the train back at the stop it had left.
	backSteps, returned int
}

// total sums the tallies of group g, or of every group for "all".
func (r result) total(g string) tally {
	var sum tally
	for name, t := range r.groups {
		if g == "all" || name == g {
			sum.seconds += t.seconds
			sum.sprints += t.sprints
			sum.leaps = append(sum.leaps, t.leaps...)
			sum.backSteps += t.backSteps
			sum.returned += t.returned
		}
	}
	slices.Sort(sum.leaps)
	return sum
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

func replay(data *static.Data, stream []input, path string, fixesAt map[int64][]int, fixes []fix, groups map[string]string, scoreFrom time.Time) result {
	config := Config{MaxAge: 10 * time.Minute, Ramp: 15 * time.Second}
	tracker := New(data, config)
	res := result{
		firstDrawn: map[stopKey]time.Time{},
		jumped:     map[stopKey]bool{},
		errors:     map[int]float64{},
		along:      map[int]float64{},
		away:       map[int]float64{},
		groups:     map[string]*tally{},
	}
	lastTarget := map[string]string{}
	lastStood := map[string]string{}
	states, lastState := map[string]drawnState{}, map[string]drawnState{}
	start, end := stream[0].fetched, stream[len(stream)-1].fetched
	i := 0
	for now := start; !now.After(end); now = now.Add(time.Second) {
		for ; i < len(stream) && !stream[i].fetched.After(now); i++ {
			if msg, ok := forPath(stream[i], path); ok {
				tracker.Apply(stream[i].feed, msg, stream[i].fetched)
			}
		}
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
					g := group(groups, feed, tr.trip)
					t := res.groups[g]
					if t == nil {
						t = &tally{}
						res.groups[g] = t
					}
					t.seconds++
					if cur.speed > sprintSpeed {
						t.sprints++
					}
					if prev, ok := lastState[key]; ok {
						d := geo.Distance(prev.at.point, cur.at.point)
						if d > 40 {
							t.leaps = append(t.leaps, d)
						}
						if d > 10 && backwards(prev, cur) {
							t.backSteps++
							if prev.moving && !cur.moving && (cur.at.stop == prev.obs.from || cur.at.stop == lastStood[key]) {
								t.returned++
							}
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

func withAll(groups map[string]bool) []string {
	return append(slices.Sorted(maps.Keys(groups)), "all")
}

func cell[K comparable](m map[K]float64, k K) string {
	if v, ok := m[k]; ok {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return ""
}

func meanAbs(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += math.Abs(v)
	}
	return sum / float64(max(1, len(values)))
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
	stream, fixes, err := loadStream(*evalCaptures, *evalMerge, groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) == 0 {
		t.Fatal("no captures")
	}
	start, end := stream[0].fetched, stream[len(stream)-1].fetched
	scoreFrom := start.Add(*evalWarmup)
	fmt.Printf("window %s – %s (%s), %d snapshots, scored from %s\n",
		start.Format("15:04:05"), end.Format("15:04:05 MST"), end.Sub(start).Round(time.Second), len(stream), scoreFrom.Format("15:04:05"))

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
	for _, p := range paths {
		wg.Go(func() {
			r := replay(data, stream, p, fixesAt, fixes, groups, scoreFrom)
			mu.Lock()
			results[p] = r
			mu.Unlock()
		})
	}
	wg.Wait()

	refs := referenceArrivals(stream, data, groups)
	type lagRow struct {
		key   stopKey
		group string
		ref   time.Time
		lag   map[string]float64
	}
	var lags []lagRow
	for k, a := range refs {
		if a.at.Before(scoreFrom) || a.at.After(end.Add(-2*time.Minute)) {
			continue
		}
		row := lagRow{k, a.group, a.at, map[string]float64{}}
		for _, p := range paths {
			if at, ok := results[p].firstDrawn[k]; ok {
				row.lag[p] = at.Sub(a.at).Seconds()
			}
		}
		lags = append(lags, row)
	}
	sort.Slice(lags, func(i, j int) bool { return lags[i].ref.Before(lags[j].ref) })

	lagGroups := map[string]bool{}
	for _, r := range lags {
		lagGroups[r.group] = true
	}
	groupNames := withAll(lagGroups)

	fmt.Println("\nARRIVAL LAG: drawn arrival minus the vehicle's first STOPPED_AT timestamp (seconds, + is late)")
	fmt.Println("Covered counts arrivals each path reached, jumped those it reached by moving the train past the stop; the lag columns use only arrivals both reached.")
	fmt.Printf("%-14s %-19s %6s %8s %7s %7s %7s %7s %7s %8s %8s\n", "group", "path", "refs", "covered", "jumped", "median", "mean|x|", "p10", "p90", ">10s er", ">30s lt")
	for _, g := range groupNames {
		both := map[string][]float64{}
		covered, jumped := map[string]int{}, map[string]int{}
		n := 0
		for _, r := range lags {
			if g != "all" && r.group != g {
				continue
			}
			n++
			for _, p := range paths {
				if _, ok := r.lag[p]; ok {
					covered[p]++
					if results[p].jumped[r.key] {
						jumped[p]++
					}
				}
			}
			if len(r.lag) == len(paths) {
				for _, p := range paths {
					both[p] = append(both[p], r.lag[p])
				}
			}
		}
		for _, p := range paths {
			v := both[p]
			sort.Float64s(v)
			fmt.Printf("%-14s %-19s %6d %7.0f%% %6.0f%% %7.0f %7.0f %7.0f %7.0f %7.1f%% %7.1f%%\n", g, p, n,
				pct(covered[p], n), pct(jumped[p], covered[p]), quantile(v, 0.5), meanAbs(v), quantile(v, 0.1), quantile(v, 0.9),
				share(v, func(x float64) bool { return x < -10 }), share(v, func(x float64) bool { return x > 30 }))
		}
	}

	fmt.Println("\nJUMPS: one-second steps where a drawn train moved more than 40m")
	fmt.Printf("%-14s %-19s %8s %7s %10s %9s %9s\n", "group", "path", "train-h", "jumps", "per tr-h", "median m", "max m")
	for _, g := range groupNames {
		for _, p := range paths {
			sum := results[p].total(g)
			hours := float64(sum.seconds) / 3600
			fmt.Printf("%-14s %-19s %8.1f %7d %10.1f %9.0f %9.0f\n", g, p, hours, len(sum.leaps), float64(len(sum.leaps))/math.Max(hours, 1e-9), quantile(sum.leaps, 0.5), quantile(sum.leaps, 1))
		}
	}

	fmt.Println("\nBACKWARDS: one-second steps of more than 10m against the direction of travel; returned counts those that put the train back at the stop it had left")
	fmt.Println("SPRINTS: share of train time drawn faster than 25 m/s")
	fmt.Printf("%-14s %-19s %8s %7s %10s %9s %8s\n", "group", "path", "train-h", "back", "per tr-h", "returned", "sprints")
	for _, g := range groupNames {
		for _, p := range paths {
			sum := results[p].total(g)
			hours := float64(sum.seconds) / 3600
			fmt.Printf("%-14s %-19s %8.1f %7d %10.2f %9d %7.1f%%\n", g, p, hours, sum.backSteps, float64(sum.backSteps)/math.Max(hours, 1e-9), sum.returned, pct(sum.sprints, sum.seconds))
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
				sort.Float64s(v)
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
				sort.Float64s(a)
				sort.Float64s(o)
				fmt.Printf("%-14s %-19s %6d %7.0f %7.0f %7.0f %7.0f %8.0f %8.0f\n", g, p, len(a),
					quantile(a, 0.5), meanAbs(a), quantile(a, 0.1), quantile(a, 0.9), quantile(o, 0.5), quantile(o, 0.9))
			}
		}
	}

	if *evalOut != "" {
		if err := os.MkdirAll(*evalOut, 0o755); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString("trip,stop,group,ref_unix,lag_a,lag_b\n")
		for _, r := range lags {
			fmt.Fprintf(&b, "%s,%s,%s,%d,%s,%s\n", r.key.trip, r.key.stop, r.group, r.ref.Unix(), cell(r.lag, pathA), cell(r.lag, pathB))
		}
		if err := os.WriteFile(filepath.Join(*evalOut, "arrivals.csv"), []byte(b.String()), 0o644); err != nil {
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
	caps, err := readCaptures(*evalCaptures)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		for _, e := range c.msg.GetEntity() {
			if v := e.GetVehicle(); v != nil && strings.Contains(v.GetTrip().GetTripId(), *evalTrace) {
				fmt.Printf("%s hdr %s  VEH %-13s %-5s ts %s\n", c.fetched.Format("15:04:05"), time.Unix(int64(c.msg.GetHeader().GetTimestamp()), 0).Format("15:04:05"),
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
				fmt.Printf("%s                TU %s\n", c.fetched.Format("15:04:05"), b.String())
			}
		}
	}
}
