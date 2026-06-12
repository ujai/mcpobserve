package main

import "testing"

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9464", true},
		{"[::1]:9464", true},
		{"localhost:9464", true},
		{"0.0.0.0:9464", false},
		{":9464", false},
		{"192.168.1.10:9464", false},
		{"no-port", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
