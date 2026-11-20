package track

import "math"

// In metres per second.
const maxSpeed = 36.0

// soonest is the least time a train at speed takes to stop togo metres on,
// cruising no faster than maxSpeed and braking no harder than maxSpeed/ramp.
func soonest(togo, speed, ramp float64) float64 {
	if ramp > 0 {
		// With no cruise, speed peaks halfway at braking*t/2 = 2*togo/t - speed/2.
		braking := maxSpeed / ramp
		if t := (math.Sqrt(speed*speed+16*braking*togo) - speed) / (2 * braking); t <= 2*ramp {
			return t
		}
	}
	return ramp + (togo-speed*ramp/2)/maxSpeed
}

// advance moves a train from distance, at speed, to stand at length after
// remaining seconds. Speed changes linearly to a cruise over ramp seconds,
// holds, then falls to zero over the last ramp. A train due sooner than it
// could manage arrives late. Distances are in metres and times in seconds.
func advance(distance, speed, length, remaining, ramp, elapsed float64) (float64, float64) {
	togo := length - distance
	if togo <= 0 {
		return length, 0
	}
	if elapsed <= 0 {
		return distance, speed
	}
	remaining = max(remaining, soonest(togo, speed, ramp))
	ramp = min(ramp, remaining/2)
	cruise := (togo - speed*ramp/2) / (remaining - ramp)

	if cruise < 0 {
		// Too close to slow down gently: brake evenly to stop at the end.
		stopIn := 2 * togo / speed
		if elapsed >= stopIn {
			return length, 0
		}
		v := speed * (1 - elapsed/stopIn)
		return distance + (speed+v)/2*elapsed, v
	}

	switch {
	case elapsed < ramp:
		v := speed + (cruise-speed)*elapsed/ramp
		return distance + (speed+v)/2*elapsed, v
	case elapsed < remaining-ramp:
		return distance + (speed+cruise)/2*ramp + cruise*(elapsed-ramp), cruise
	case elapsed < remaining:
		left := remaining - elapsed
		v := cruise * left / ramp
		return length - v*left/2, v
	}
	return length, 0
}
