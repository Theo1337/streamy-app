package stack

import "testing"

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
