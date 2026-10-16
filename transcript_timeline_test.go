package main

import (
	"strings"
	"testing"
	"time"
)

func TestTranscriptTimelinePreservesOverlappingSpeakers(t *testing.T) {
	second := int64(time.Second)
	lines := []transcriptLine{
		{ID: 1, Source: 0, Name: "Друг", At: second, EndAt: 6 * second, Text: strings.Repeat("Длинная реплика ", 30)},
		{ID: 2, Source: 1, Name: "Друг", At: 2 * second, EndAt: 3 * second, Text: "Перебиваю"},
		{ID: 3, Source: 0, Name: "Друг", At: 6 * second, EndAt: 7 * second, Text: "Продолжение"},
		{ID: 4, Source: 2, Name: "Хост", At: second, EndAt: 4 * second, Text: "Одновременно"},
	}
	l := layoutTranscript(lines, 18, 8, func(text string) int { return len([]rune(text)) + 50 })
	if len(l.Lanes) != 3 {
		t.Fatal("equal names merged independent sources", l.Lanes)
	}
	a, b, c, d := l.Cards[0], l.Cards[1], l.Cards[2], l.Cards[3]
	if a.Top != d.Top {
		t.Fatal("simultaneous speech is not aligned", a, d)
	}
	if b.Top <= a.Top || b.Bottom >= a.Bottom {
		t.Fatal("interruption is outside original speech interval", a, b)
	}
	if c.Top < a.Bottom {
		t.Fatal("same speaker's complete text overlaps next utterance", a, c)
	}
	if a.Bottom-a.Top < len([]rune(a.Line.Text))+50 {
		t.Fatal("long text was clipped")
	}
}

func TestTranscriptTimelineHandlesLateResultsAndPreviewGrowth(t *testing.T) {
	second := int64(time.Second)
	lines := []transcriptLine{
		{ID: 2, Source: 1, At: 2 * second, EndAt: 4 * second, Text: "Результат пришёл первым"},
		{ID: 1, Source: 0, At: second, EndAt: 5 * second, Text: "Поздний результат", Partial: true},
	}
	measure := func(s string) int { return len([]rune(s)) + 30 }
	l := layoutTranscript(lines, 18, 8, measure)
	if l.Cards[1].Top >= l.Cards[0].Top {
		t.Fatal("late result ordered by arrival instead of audio time")
	}
	lines[1].Text = strings.Repeat("Уточнённый текст ", 30)
	l = layoutTranscript(lines, 18, 8, measure)
	if l.Cards[1].Bottom-l.Cards[1].Top < measure(lines[1].Text) {
		t.Fatal("growing preview clipped")
	}
}
