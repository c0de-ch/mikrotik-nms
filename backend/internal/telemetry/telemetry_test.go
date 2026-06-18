package telemetry

import "testing"

func TestParseHeaders(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"empty", "", nil},
		{"whitespace", "   ", nil},
		{"single", "Authorization=Bearer xyz", map[string]string{"Authorization": "Bearer xyz"}},
		{"multi + trim", " a = 1 , b=2 ", map[string]string{"a": "1", "b": "2"}},
		{"skips malformed", "good=1,bad,also=2", map[string]string{"good": "1", "also": "2"}},
		{"value may contain =", "k=a=b", map[string]string{"k": "a=b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseHeaders(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("ParseHeaders(%q) = %v, want %v", c.in, got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestConfigIsHTTP(t *testing.T) {
	if !(Config{Protocol: "http"}).isHTTP() || !(Config{Protocol: "HTTP"}).isHTTP() {
		t.Error("expected http protocol to be detected case-insensitively")
	}
	if (Config{Protocol: "grpc"}).isHTTP() || (Config{Protocol: ""}).isHTTP() {
		t.Error("expected non-http protocols to default to gRPC")
	}
}
