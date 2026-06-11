package main

import "testing"

func TestLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{":8080", false},        // empty host binds all interfaces
		{"0.0.0.0:8080", false}, // explicit wildcard
		{"[::]:8080", false},
		{"192.168.1.10:8080", false},
		{"example.com:8080", false},
		{"localhost:8080", true},
		{"127.0.0.1:8080", true},
		{"127.0.0.53:8080", true}, // any 127/8 address is loopback
		{"[::1]:8080", true},
		{"8080", false},    // not host:port; fail closed (warn)
		{"garbage", false}, // unparseable; fail closed
	}
	for _, tt := range tests {
		if got := loopbackAddr(tt.addr); got != tt.want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
