package vncfeedback

import "testing"

func TestFeedbackRoundTripAndBounds(t *testing.T) {
	r := Report{true, 1000, 30, 22, 18000, 3}
	got, err := Parse(r.Marshal())
	if err != nil || got != r {
		t.Fatal(got, err)
	}
	for _, bad := range []Report{{IntervalMS: 0}, {IntervalMS: 10001}, {IntervalMS: 1000, Frames: 10001}, {IntervalMS: 1000, DecodeUS: 10000001}, {IntervalMS: 1000, Pending: 10001}} {
		if _, e := Parse(bad.Marshal()); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	for _, i := range []int{0, 1, 2, 3} {
		b := r.Marshal()
		b[i] = 255
		if _, e := Parse(b); e == nil {
			t.Fatal("accepted invalid header", i)
		}
	}
	if _, e := Parse(r.Marshal()[:20]); e == nil {
		t.Fatal("truncated report accepted")
	}
}
