package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestCounterVecRendersSortedEscapedSeries(t *testing.T) {
	v := NewCounterVec("test_events_total", "Events.\nSecond line.", "kind", "result")
	v.Inc("b", "ok")
	v.Add(3, "a", `quo"te`)
	v.Add(0, "c", "ignored") // non-positive adds never create a series
	v.Inc("b", "ok")

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.CounterVec(v)
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	want := "# HELP test_events_total Events.\\nSecond line.\n" +
		"# TYPE test_events_total counter\n" +
		"test_events_total{kind=\"a\",result=\"quo\\\"te\"} 3\n" +
		"test_events_total{kind=\"b\",result=\"ok\"} 2\n"
	if got := buf.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := v.Value("b", "ok"); got != 2 {
		t.Fatalf("Value = %d, want 2", got)
	}
}

func TestCounterIgnoresNegativeAdds(t *testing.T) {
	c := NewCounter("test_bytes_total", "Bytes.")
	c.Add(10)
	c.Add(-5)
	c.Inc()
	if got := c.Value(); got != 11 {
		t.Fatalf("Value = %d, want 11", got)
	}
}

func TestGauges(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Gauge("test_up", "Up.", 1)
	w.GaugeVec("test_things", "Things.", "state", map[string]float64{"z": 2, "a": 0.5})
	out := buf.String()
	for _, line := range []string{
		"# TYPE test_up gauge\ntest_up 1\n",
		"test_things{state=\"a\"} 0.5\ntest_things{state=\"z\"} 2\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q:\n%s", line, out)
		}
	}
}
