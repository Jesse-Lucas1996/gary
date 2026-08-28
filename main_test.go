package main

import (
	"slices"
	"testing"
)

// reorder hoists flags ahead of positionals so they can sit anywhere on the
// line. A boolean flag missing from its table silently swallows the next token
// — which for `send` and `post` is the message body itself.
func TestReorderHoistsFlagsWithoutEatingPositionals(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			"a boolean flag leaves the message body alone",
			[]string{"coder", "--from", "planner", "--expect-reply", "what broke the lexer?"},
			[]string{"--from", "planner", "--expect-reply", "coder", "what broke the lexer?"},
		},
		{
			"a valued flag takes its value with it",
			[]string{"standup", "--description", "daily status"},
			[]string{"--description", "daily status", "standup"},
		},
		{
			"a terminator ends flag parsing",
			[]string{"coder", "--from", "planner", "--", "--not-a-flag"},
			[]string{"--from", "planner", "coder", "--not-a-flag"},
		},
	} {
		if got := reorder(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
