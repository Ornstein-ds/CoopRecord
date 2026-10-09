package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

func TestMixerGain(t *testing.T) {
	for _, test := range []struct{ position, gain int32 }{
		{-1, 200}, {0, 200}, {mixerUnityPosition, 100}, {100, 0}, {101, 0},
	} {
		if got := mixerGain(test.position); got != test.gain {
			t.Fatalf("position %d: gain %d, want %d", test.position, got, test.gain)
		}
	}
	for position := int32(0); position <= 100; position++ {
		gain := mixerGain(position)
		if mixerPosition(gain) != position || gain > mixerGain(position-1) {
			t.Fatal("gain mapping jumps or reverses", position, gain)
		}
	}
	values := []int16{-32768, -20000, -1000, 0, 1000, 20000, 32767}
	src, dst := make([]byte, len(values)*2), make([]byte, len(values)*2)
	for i, value := range values {
		binary.LittleEndian.PutUint16(src[2*i:], uint16(value))
	}
	original := bytes.Clone(src)
	for _, test := range []struct {
		gain int32
		want []int16
	}{
		{0, []int16{0, 0, 0, 0, 0, 0, 0}},
		{25, []int16{-8192, -5000, -250, 0, 250, 5000, 8191}},
		{100, values},
		{150, []int16{-32768, -30000, -1500, 0, 1500, 30000, 32767}},
		{200, []int16{-32768, -32768, -2000, 0, 2000, 32767, 32767}},
		{100, values},
	} {
		applyMixerGain(dst, src, test.gain)
		for i, want := range test.want {
			if got := int16(binary.LittleEndian.Uint16(dst[2*i:])); got != want {
				t.Fatalf("gain %d, sample %d: got %d, want %d", test.gain, values[i], got, want)
			}
		}
		if !bytes.Equal(src, original) {
			t.Fatal("monitor gain changed source audio")
		}
	}
}

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
