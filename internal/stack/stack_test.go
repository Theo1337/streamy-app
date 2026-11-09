package stack

import (
	"errors"
	"testing"
)

func TestFinishAllowsRestartAfterFailure(t *testing.T) {
	m := &Manager{started: true, generation: 3}

	m.finish(3, errors.New("exit status 1"), false)

	if m.started {
		t.Fatal("started is still true after a failed start; a later Start would be ignored")
	}
	if m.running {
		t.Fatal("running is true after a failed start")
	}
	if m.err == nil {
		t.Fatal("the startup error was not recorded")
	}
}

func TestFinishKeepsStartedForARunningChild(t *testing.T) {
	m := &Manager{started: true, generation: 2}

	m.finish(2, nil, true)

	if !m.started {
		t.Fatal("started was cleared while the child is running")
	}
	if !m.running {
		t.Fatal("running was not set")
	}
}

func TestFinishIgnoresAStaleGeneration(t *testing.T) {
	m := &Manager{started: true, generation: 4}

	m.finish(3, errors.New("exit status 1"), false)

	if !m.started {
		t.Fatal("a stale finish cleared the state of a newer generation")
	}
}

func TestPortFromNotifyURL(t *testing.T) {
	cases := map[string]int{
		"":                                   defaultBotPort,
		"not a url":                          defaultBotPort,
		"http://127.0.0.1/api/notify":        defaultBotPort,
		"http://127.0.0.1:9000/api/notify":   9000,
		"http://127.0.0.1:99999/api/notify":  defaultBotPort,
		"https://example.com:443/api/notify": 443,
	}
	for input, want := range cases {
		if got := portFromNotifyURL(input); got != want {
			t.Errorf("portFromNotifyURL(%q) = %d, want %d", input, got, want)
		}
	}
}
