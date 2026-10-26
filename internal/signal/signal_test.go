package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNewRoomIDsMatchProtocol(t *testing.T) {
	room, token, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	if len(room) != 16 || len(token) != 43 {
		t.Fatalf("ID lengths = %d/%d, want 16/43", len(room), len(token))
	}
}

func TestViewerLinkKeepsCredentialsInFragment(t *testing.T) {
	got, err := ViewerLink("https://watch.example/live", "wss://signal.example", "room-id", "secret")
	if err != nil {
		t.Fatalf("ViewerLink: %v", err)
	}
	if strings.Contains(got, "?") {
		t.Fatalf("link put credentials in query: %s", got)
	}
	if want := "https://watch.example/live#room=room-id&signal=wss%3A%2F%2Fsignal.example&token=secret"; got != want {
		t.Errorf("viewer link = %q, want %q", got, want)
	}
}

func TestLobbyLinkRemovesPrivateURLParameters(t *testing.T) {
	got, err := LobbyLink("https://watch.example/live?token=private#old-fragment")
	if err != nil {
		t.Fatalf("LobbyLink: %v", err)
	}
	if got != "https://watch.example/live" {
		t.Fatalf("LobbyLink = %q, want a credential-free lobby URL", got)
	}
}

func TestViewerLinkCarriesICEConfigurationOnlyInFragment(t *testing.T) {
	got, err := ViewerLinkWithICE(
		"https://watch.example/live", "wss://signal.example", "room-id", "secret",
		[]ICEServer{{URLs: []string{"turn:relay.example"}, Username: "user", Credential: "password"}},
	)
	if err != nil {
		t.Fatalf("ViewerLinkWithICE: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RawQuery != "" {
		t.Fatalf("link has query parameters: %q", parsed.RawQuery)
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	var servers []ICEServer
	if err := json.Unmarshal([]byte(fragment.Get("ice")), &servers); err != nil {
		t.Fatalf("parse ICE servers from fragment: %v", err)
	}
	if len(servers) != 1 || servers[0].Credential != "password" {
		t.Fatalf("ICE servers = %+v, want the configured TURN relay", servers)
	}
	if !strings.Contains(parsed.EscapedFragment(), "password") {
		t.Fatal("TURN credentials are missing from the viewer URL fragment")
	}
}

func TestWebSocketClientConnectsSendsAndReceives(t *testing.T) {
	const room = "abcdefghijklmnop"
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"
	serverErr := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/room/"+room+"/ws"; got != want {
			serverErr <- &testError{message: "path = " + got + ", want " + want}
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		var hello Message
		if err := conn.ReadJSON(&hello); err != nil {
			serverErr <- err
			return
		}
		if hello.Type != "hello" || hello.Role != RoleHost || hello.Token != token || hello.MaxViewers != 4 {
			serverErr <- &testError{message: "unexpected hello: " + mustJSON(hello)}
			return
		}
		if err := conn.WriteJSON(Message{Version: ProtocolVersion, Type: "welcome", Room: room, PeerID: "abcdefgh", Role: RoleHost}); err != nil {
			serverErr <- err
			return
		}
		var sent Message
		if err := conn.ReadJSON(&sent); err != nil {
			serverErr <- err
			return
		}
		if sent.Type != "offer" || sent.To != "viewer-id" || sent.SDP != "v=0\r\n" {
			serverErr <- &testError{message: "unexpected sent offer: " + mustJSON(sent)}
			return
		}
		if err := conn.WriteJSON(Message{Version: ProtocolVersion, Type: "peer-joined", PeerID: "viewer-id", Role: RoleViewer}); err != nil {
			serverErr <- err
			return
		}
		_, _, _ = conn.ReadMessage()
		serverErr <- nil
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewWebSocket(ctx, endpoint, room, token, 4)
	if err != nil {
		t.Fatalf("NewWebSocket: %v", err)
	}
	defer client.Close()

	welcome := <-client.Messages()
	if welcome.Type != "welcome" || welcome.PeerID != "abcdefgh" {
		t.Fatalf("welcome = %+v", welcome)
	}
	if err := client.Send(ctx, Message{Type: "offer", To: "viewer-id", SDP: "v=0\r\n"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case joined := <-client.Messages():
		if joined.Type != "peer-joined" || joined.PeerID != "viewer-id" {
			t.Fatalf("message = %+v", joined)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for peer-joined")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }
