package track

import (
	"math"
	"testing"
)

func TestAdvanceFromStandstill(t *testing.T) {
	// 900 m in 75 s with 15 s ramps cruises at 900 / (75 - 15) = 15 m/s.
	cases := []struct {
		elapsed, distance, speed float64
	}{
		{0, 0, 0},
		{7.5, 28.125, 7.5},
		{15, 112.5, 15},
		{37.5, 450, 15},
		{60, 787.5, 15},
		{67.5, 871.875, 7.5},
		{75, 900, 0},
		{500, 900, 0},
	}
	for _, c := range cases {
		distance, speed := advance(0, 0, 900, 75, 15, c.elapsed)
		if math.Abs(distance-c.distance) > 1e-6 || math.Abs(speed-c.speed) > 1e-6 {
			t.Errorf("after %.1f s: %.4f m at %.2f m/s, want %.4f m at %.2f m/s", c.elapsed, distance, speed, c.distance, c.speed)
		}
	}
}

func TestAdvanceWithoutRampIsLinear(t *testing.T) {
	distance, speed := advance(250, 99, 1000, 60, 0, 30)
	if math.Abs(distance-625) > 1e-6 || math.Abs(speed-12.5) > 1e-6 {
		t.Errorf("got %.2f m at %.2f m/s, want 625 m at 12.5 m/s", distance, speed)
	}
}

func TestAdvanceIsSmoothFromAnyStart(t *testing.T) {
	starts := []struct {
		name                               string
		distance, speed, length, remaining float64
	}{
		{"standing start", 0, 0, 800, 90},
		{"mid-run with a later arrival", 400, 12, 800, 200},
		{"mid-run with an earlier arrival", 400, 6, 800, 20},
		{"short hop", 0, 0, 120, 20},
		{"arrival already due", 300, 10, 800, -30},
		{"fast and nearly there", 790, 15, 800, 40},
	}
	const ramp, tick = 15.0, 0.1
	for _, s := range starts {
		lastDistance, lastSpeed := s.distance, s.speed
		reached := false
		for elapsed := tick; elapsed < 400; elapsed += tick {
			distance, speed := advance(s.distance, s.speed, s.length, s.remaining, ramp, elapsed)
			if distance < lastDistance-1e-9 || distance > s.length+1e-9 {
				t.Fatalf("%s: distance went from %.3f to %.3f after %.1f s", s.name, lastDistance, distance, elapsed)
			}
			if speed < -1e-9 || speed > maxSpeed+1e-9 {
				t.Fatalf("%s: speed %.2f m/s after %.1f s", s.name, speed, elapsed)
			}
			// With no cruise, the speed peaks between two samples, a little
			// above both.
			if step := distance - lastDistance; step > (max(speed, lastSpeed)+0.5)*tick {
				t.Fatalf("%s: moved %.3f m in one tick at %.2f m/s after %.1f s", s.name, step, speed, elapsed)
			}
			if !reached && distance >= s.length {
				reached = true
				// A train too close to brake over the full ramp stops early.
				canBrake := s.speed*ramp/2 <= s.length-s.distance
				if canBrake && s.remaining > soonest(s.length-s.distance, s.speed, ramp) && math.Abs(elapsed-s.remaining) > tick {
					t.Errorf("%s: arrived after %.1f s, want %.1f s", s.name, elapsed, s.remaining)
				}
			}
			lastDistance, lastSpeed = distance, speed
		}
		if !reached {
			t.Errorf("%s: never arrived", s.name)
		}
	}
}

func TestRemakingThePlanOfAMovingTrainKeepsItsArrival(t *testing.T) {
	const length, due, ramp, tick = 700.0, 75.0, 15.0, 0.1
	distance, speed, since := 0.0, 0.0, 0.0
	for now := tick; now < 200; now += tick {
		d, v := advance(distance, speed, length, due-since, ramp, now-since)
		if d >= length {
			if math.Abs(now-due) > 2*tick {
				t.Errorf("arrived after %.1f s, want %.1f s", now, due)
			}
			return
		}
		if now-since >= 5 {
			distance, speed, since = d, v, now
		}
	}
	t.Error("never arrived")
}

func TestOverdueTrainAlreadyMovingRunsStraightIn(t *testing.T) {
	// From a standing start 150m takes 150/36 + 15 = 19.2 s.
	const tick = 0.1
	for elapsed := tick; elapsed < 60; elapsed += tick {
		if distance, _ := advance(650, 15, 800, 0, 15, elapsed); distance >= 800 {
			if elapsed > 14 {
				t.Errorf("arrived after %.1f s, want under 14 s", elapsed)
			}
			return
		}
	}
	t.Error("never arrived")
}
