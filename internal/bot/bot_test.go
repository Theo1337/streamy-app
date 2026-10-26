package bot

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type fakeClient struct {
	channels map[string]*discordgo.Channel
	messages map[string][]*discordgo.Message
	sent     []*discordgo.MessageSend
	deleted  []string
	nextID   int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		channels: map[string]*discordgo.Channel{
			"123456": {ID: "123456", Type: discordgo.ChannelTypeGuildText},
		},
		messages: make(map[string][]*discordgo.Message),
	}
}

func (f *fakeClient) Channel(channelID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	channel, ok := f.channels[channelID]
	if !ok {
		return nil, errors.New("unknown channel")
	}
	return channel, nil
}

func (f *fakeClient) ChannelMessages(channelID string, _ int, _, _, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	return f.messages[channelID], nil
}

func (f *fakeClient) ChannelMessage(channelID, messageID string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	for _, message := range f.messages[channelID] {
		if message.ID == messageID {
			return message, nil
		}
	}
	return nil, errors.New("unknown message")
}

func (f *fakeClient) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.nextID++
	message := &discordgo.Message{
		ID:        "msg-" + string(rune('0'+f.nextID)),
		ChannelID: channelID,
		Content:   data.Content,
		Author:    &discordgo.User{ID: "bot-1"},
	}
	f.sent = append(f.sent, data)
	f.messages[channelID] = append(f.messages[channelID], message)
	return message, nil
}

func (f *fakeClient) ChannelMessageDelete(channelID, messageID string, _ ...discordgo.RequestOption) error {
	f.deleted = append(f.deleted, messageID)
	kept := f.messages[channelID][:0]
	for _, message := range f.messages[channelID] {
		if message.ID != messageID {
			kept = append(kept, message)
		}
	}
	f.messages[channelID] = kept
	return nil
}

func newTestBot(client discordClient) *bot {
	return newBot(client, "bot-1", Config{
		APIKey:         "secret",
		WatchChannelID: "123456",
		ViewerURL:      "https://viewer.example/",
		Port:           DefaultPort,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post(t *testing.T, handler http.Handler, path, body, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return value
}

func TestNotifyStartAndStop(t *testing.T) {
	client := newFakeClient()
	b := newTestBot(client)
	handler := b.routes()

	start := post(t, handler, "/api/notify",
		`{"action":"start","channelId":"123456","streamer":"A*B","url":"https://example.com/?room=1"}`,
		"Bearer secret")
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d, body %s", start.Code, start.Body)
	}
	if got := decode(t, start)["messageId"]; got != "msg-1" {
		t.Fatalf("messageId = %v, want msg-1", got)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(client.sent))
	}
	want := `**A\*B** is live. Watch here: https://example.com/?room=1`
	if client.sent[0].Content != want {
		t.Fatalf("content = %q, want %q", client.sent[0].Content, want)
	}
	if client.sent[0].AllowedMentions == nil || len(client.sent[0].AllowedMentions.Parse) != 0 {
		t.Fatalf("allowed mentions = %+v, want an empty parse list", client.sent[0].AllowedMentions)
	}

	stop := post(t, handler, "/api/notify", `{"action":"stop","channelId":"123456"}`, "Bearer secret")
	if stop.Code != http.StatusOK {
		t.Fatalf("stop status = %d, body %s", stop.Code, stop.Body)
	}
	if got := decode(t, stop)["deleted"]; got != float64(1) {
		t.Fatalf("deleted = %v, want 1", got)
	}
	if len(client.deleted) != 1 || client.deleted[0] != "msg-1" {
		t.Fatalf("deleted = %v, want [msg-1]", client.deleted)
	}
}

func TestNotifyRequiresAuthorization(t *testing.T) {
	b := newTestBot(newFakeClient())
	response := post(t, b.routes(), "/api/notify", `{"channelId":"123456"}`, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestNotifyRejectsNonHTTPViewerURL(t *testing.T) {
	b := newTestBot(newFakeClient())
	response := post(t, b.routes(), "/api/notify",
		`{"channelId":"123456","url":"javascript:alert(1)"}`, "Bearer secret")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestNotifyUnknownChannelIsNotFound(t *testing.T) {
	b := newTestBot(newFakeClient())
	b.cfg.WatchChannelID = "999999"
	response := post(t, b.routes(), "/api/notify",
		`{"channelId":"999999","url":"https://example.com/"}`,
		"Bearer secret")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestNotifyFallsBackToWatchChannel(t *testing.T) {
	client := newFakeClient()
	b := newTestBot(client)
	response := post(t, b.routes(), "/api/notify",
		`{"url":"https://example.com/room"}`, "Bearer secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	if got := decode(t, response)["channelId"]; got != "123456" {
		t.Fatalf("channelId = %v, want the watch channel", got)
	}
}

func TestHealthz(t *testing.T) {
	b := newTestBot(newFakeClient())
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	b.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok\n" {
		t.Fatalf("health = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestQuit(t *testing.T) {
	b := newTestBot(newFakeClient())
	response := post(t, b.routes(), "/api/quit", "", "Bearer secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	select {
	case <-b.quit:
	default:
		t.Fatal("quit channel was not closed")
	}
}

func TestViewerURL(t *testing.T) {
	b := newTestBot(newFakeClient())
	cases := map[string]string{
		"https://example.com/?room=1": "https://example.com/?room=1",
		"http://localhost:8080/x":     "http://localhost:8080/x",
		"javascript:alert(1)":         "",
		"not a url":                   "",
		"/relative":                   "",
		"":                            "https://viewer.example/",
	}
	for input, want := range cases {
		if got := b.viewerURL(input); got != want {
			t.Errorf("viewerURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEscapeMarkdown(t *testing.T) {
	want := "a\\*b\\_c\\`d\\~e\\\\f"
	if got := escapeMarkdown("a*b_c`d~e\\f"); got != want {
		t.Fatalf("escapeMarkdown = %q, want %q", got, want)
	}
}

func TestIsLiveMessage(t *testing.T) {
	cases := map[string]bool{
		"Streamy is live. Watch here: https://example.com": true,
		"**Ana** is live. Watch here: https://example.com": true,
		"hello":                                  false,
		"is live. Watch here: ftp://example.com": false,
	}
	for input, want := range cases {
		if got := isLiveMessage(input); got != want {
			t.Errorf("isLiveMessage(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestEqualStrings(t *testing.T) {
	if !equalStrings("same", "same") {
		t.Fatal("equal strings reported different")
	}
	if equalStrings("same", "different") || equalStrings("", "x") {
		t.Fatal("different strings reported equal")
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		"STREAMY_TEST_A=from-file",
		"# comment",
		"STREAMY_TEST_B=\"two words\"",
		"STREAMY_TEST_C='three'",
		"BAD KEY=ignored",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"STREAMY_TEST_A", "STREAMY_TEST_B", "STREAMY_TEST_C"} {
		os.Unsetenv(key)
		t.Cleanup(func() { os.Unsetenv(key) })
	}
	t.Setenv("STREAMY_TEST_A", "from-env")

	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("STREAMY_TEST_A"); got != "from-env" {
		t.Errorf("existing value was overwritten: %q", got)
	}
	if got := os.Getenv("STREAMY_TEST_B"); got != "two words" {
		t.Errorf("double quoted value = %q", got)
	}
	if got := os.Getenv("STREAMY_TEST_C"); got != "three" {
		t.Errorf("single quoted value = %q", got)
	}
}

func TestFindEnvFileHonorsExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if got := FindEnvFile(path); got != path {
		t.Fatalf("FindEnvFile = %q, want %q", got, path)
	}
}
