package main

import (
	"context"
	"testing"
)

func TestMixerRosterAndMeter(t *testing.T) {
	v := &voiceSession{source: 2, ctx: context.Background()}
	for _, members := range [][]rosterMember{
		nil, {{0, "Host"}}, {{0, "Host"}, {2, "Guest"}, {2, "Duplicate"}},
		{{0, "Host"}, {2, "\x00"}}, {{0, "Host"}, {2, " "}},
	} {
		if v.setRoster(members) == nil {
			t.Fatal("accepted invalid roster", members)
		}
	}
	if err := v.setRoster([]rosterMember{{0, "Same name"}, {1, "Same name"}, {2, "Me"}}); err != nil {
		t.Fatal(err)
	}
	if len(v.channels) != 2 || v.channels[0] == v.channels[1] || v.channels[2] != nil {
		t.Fatal("channels confused equal names or self")
	}
	c := v.channels[0]
	if c.volume.Load() != 100 {
		t.Fatal("wrong default gain")
	}
	if n := pcmPeak([]byte{0, 0, 0, 128, 0xff, 0x7f}); n != 100 {
		t.Fatal("full-scale peak", n)
	}
	c.meter(20)
	c.meter(80)
	c.meter(10)
	if c.peak.Swap(0) != 80 || c.peak.Load() != 0 {
		t.Fatal("meter did not hold/consume peak")
	}
}
