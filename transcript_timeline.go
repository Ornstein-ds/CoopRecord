package main

import (
	"sort"
	"time"
)

type transcriptLane struct {
	Source int
	Name   string
}
type transcriptCard struct {
	Line              transcriptLine
	Lane, Top, Bottom int
}
type transcriptTick struct {
	At   int64
	Time int64
	Y    int
}
type transcriptLayout struct {
	Lanes  []transcriptLane
	Cards  []transcriptCard
	Ticks  []transcriptTick
	Height int
}

// Every lane uses the same mapping of audio time to vertical position. Text may
// expand an interval, but that expansion applies to all speakers, preserving
// overlapping speech while ensuring complete, readable cards in each lane.
func layoutTranscript(lines []transcriptLine, pixelsPerSecond float64, padding int, measure func(string) int) transcriptLayout {
	var out transcriptLayout
	names := map[int]string{}
	for _, l := range lines {
		if l.Text != "" {
			names[l.Source] = l.Name
		}
	}
	for source, name := range names {
		out.Lanes = append(out.Lanes, transcriptLane{source, name})
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Source < out.Lanes[j].Source })
	lanes := map[int]int{}
	for i, lane := range out.Lanes {
		lanes[lane.Source] = i
	}
	var points []int64
	ends := map[int64][]int{}
	tickTimes := map[int64]int64{}
	for _, l := range lines {
		if l.Text == "" {
			continue
		}
		if l.EndAt <= l.At {
			l.EndAt = l.At + int64(20*time.Millisecond)
		}
		index := len(out.Cards)
		out.Cards = append(out.Cards, transcriptCard{Line: l, Lane: lanes[l.Source]})
		points = append(points, l.At, l.EndAt)
		tickTimes[l.At] = l.Time
		ends[l.EndAt] = append(ends[l.EndAt], index)
	}
	if len(points) == 0 {
		return out
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	positions := map[int64]int{}
	previous, y := points[0], padding
	for _, stamp := range points {
		if _, exists := positions[stamp]; exists {
			continue
		}
		y += int(float64(stamp-previous) / float64(time.Second) * pixelsPerSecond)
		for _, index := range ends[stamp] {
			card := &out.Cards[index]
			y = max(y, positions[card.Line.At]+measure(card.Line.Text)+padding)
		}
		positions[stamp] = y
		previous = stamp
	}
	for i := range out.Cards {
		c := &out.Cards[i]
		c.Top, c.Bottom = positions[c.Line.At], positions[c.Line.EndAt]-padding
	}
	// Ticks at utterance starts remain useful when text expands a short interval.
	for _, stamp := range points {
		if elapsed, exists := tickTimes[stamp]; exists && (len(out.Ticks) == 0 || out.Ticks[len(out.Ticks)-1].At != stamp) {
			out.Ticks = append(out.Ticks, transcriptTick{At: stamp, Time: elapsed, Y: positions[stamp]})
		}
	}
	out.Height = y + padding
	return out
}
