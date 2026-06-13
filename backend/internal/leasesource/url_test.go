package leasesource

import (
	"reflect"
	"testing"
)

func TestSplitURLs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"http://a:8000", []string{"http://a:8000"}},
		{"http://a:8000\nhttp://b:8000", []string{"http://a:8000", "http://b:8000"}},
		{"http://a:8000, http://b:8000", []string{"http://a:8000", "http://b:8000"}},
		{"http://a:8000,\n\n http://b:8000 \r\n", []string{"http://a:8000", "http://b:8000"}},
	}
	for _, c := range cases {
		if got := splitURLs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitURLs(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
