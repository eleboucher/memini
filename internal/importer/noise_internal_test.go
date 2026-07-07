package importer

import "testing"

func TestIsNoiseFraming(t *testing.T) {
	// Ground truth: the exact shape found captured in production — the "User:"
	// label defeated a bare prefix check and let this into the corpus.
	cron := "User: [cron:b571428f-243c-4604-919e-effb800d44c0 homelab-peers-commits] " +
		"You are a homelab commit watcher. Check these repos and post to Discord."
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"production cron behind User: label", cron, true},
		{"bare cron marker", "[cron:x job] do a thing", true},
		{"subagent context behind label", "Assistant: [Subagent Context] delegated task", true},
		{"real message mentioning a marker is kept", "User: how do I write a [cron: job]?", false},
		{"plain message", "please set up backups", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoiseFraming(tc.in); got != tc.want {
				t.Fatalf("isNoiseFraming(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripRuntimePreambles(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"fenced json block",
			"Conversation info (untrusted metadata):\n```json\n{\"chat_id\":1}\n```\nhello there",
			"hello there",
		},
		{
			"flat key=value block",
			"Chat (untrusted metadata):\nchat_id=C1\nmessage_id=M2\n\nUser: real message",
			"User: real message",
		},
		{
			"stacked blocks",
			"A (untrusted metadata):\n```\n{}\n```\nB (untrusted metadata):\n```json\n{}\n```\nactual",
			"actual",
		},
		{"metadata only leaves nothing", "X (untrusted metadata):\n```json\n{}\n```", ""},
		{"real message with = is untouched", "User: set FOO=bar please", "User: set FOO=bar please"},
		{"plain message untouched", "plain message", "plain message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripRuntimePreambles(tc.in); got != tc.want {
				t.Fatalf("stripRuntimePreambles(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
