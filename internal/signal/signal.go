// Package signal implements the room-based WebSocket signaling protocol.
package signal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const ProtocolVersion = 1

// Role identifies a signaling peer.
type Role string

const (
	RoleHost   Role = "host"
	RoleViewer Role = "viewer"
)

// Message is one versioned JSON envelope. Optional fields stay zero-valued and
// are omitted when not relevant to the message type.
type Message struct {
	Version       int             `json:"v"`
	Type          string          `json:"type"`
	Role          Role            `json:"role,omitempty"`
	IdentityToken string          `json:"identityToken,omitempty"`
	Room          string          `json:"room,omitempty"`
	PeerID        string          `json:"peerID,omitempty"`
	From          string          `json:"from,omitempty"`
	To            string          `json:"to,omitempty"`
	Token         string          `json:"token,omitempty"`
	HostPresent   bool            `json:"hostPresent,omitempty"`
	ViewerCount   int             `json:"viewerCount,omitempty"`
	MaxViewers    int             `json:"maxViewers,omitempty"`
	SDP           string          `json:"sdp,omitempty"`
	Candidate     string          `json:"candidate,omitempty"`
	SDPMid        *string         `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16         `json:"sdpMLineIndex,omitempty"`
	Code          string          `json:"code,omitempty"`
	ErrorMessage  string          `json:"message,omitempty"`
	Fatal         *bool           `json:"fatal,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Time          int64           `json:"t,omitempty"`
	ICE           []ICEServer     `json:"iceServers,omitempty"`
	Capabilities  json.RawMessage `json:"capabilities,omitempty"`
}

// ICEServer matches WebRTC's RTCIceServer JSON shape.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Signaler is the boundary between streaming and its signaling transport.
type Signaler interface {
	Connect(ctx context.Context, room, token string, maxViewers int) error
	Messages() <-chan Message
	Send(ctx context.Context, message Message) error
	Room() string
	Token() string
	Err() error
	Close() error
}

// WebSocket is the signaling client for the Streamy Worker.
type WebSocket struct {
	endpoint  string
	conn      *websocket.Conn
	room      string
	token     string
	peerID    string
	role      Role
	messages  chan Message
	done      chan struct{}
	stop      chan struct{}
	sendMu    sync.Mutex
	closeOnce sync.Once
	readErr   error
}

// New creates a disconnected signaling client for endpoint.
func New(endpoint string) *WebSocket {
	return &WebSocket{
		endpoint: endpoint,
		messages: make(chan Message, 64),
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
	}
}

// NewRoom creates protocol-conforming cryptographically random identifiers.
func NewRoom() (room, token string, err error) {
	room, err = randomID(12)
	if err != nil {
		return "", "", fmt.Errorf("create room ID: %w", err)
	}
	token, err = randomID(32)
	if err != nil {
		return "", "", fmt.Errorf("create room token: %w", err)
	}
	return room, token, nil
}

func randomID(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ViewerLink builds the link whose room and token live only in its fragment.
func ViewerLink(viewerBaseURL, signalURL, room, token string) (string, error) {
	return ViewerLinkWithICE(viewerBaseURL, signalURL, room, token, nil)
}

// LobbyLink returns the shared public lobby URL without publishing any
// per-stream room credentials.
func LobbyLink(viewerBaseURL string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(viewerBaseURL))
	if err != nil {
		return "", fmt.Errorf("parse signaling.viewerURL: %w", err)
	}
	if (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return "", errors.New("signaling.viewerURL must be an absolute http or https URL")
	}
	base.RawQuery = ""
	base.Fragment = ""
	base.RawFragment = ""
	return base.String(), nil
}

// ViewerLinkWithICE includes relay configuration in the fragment so the
// browser can use the same STUN/TURN setup as the host without sending
// credentials to the static viewer host or signaling Worker.
func ViewerLinkWithICE(viewerBaseURL, signalURL, room, token string, iceServers []ICEServer) (string, error) {
	base, err := url.Parse(strings.TrimSpace(viewerBaseURL))
	if err != nil {
		return "", fmt.Errorf("parse signaling.viewerURL: %w", err)
	}
	if (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return "", errors.New("signaling.viewerURL must be an absolute http or https URL")
	}
	base.Fragment = ""
	base.RawFragment = ""
	if room == "" || token == "" {
		return "", errors.New("room and token are required to build the viewer link")
	}
	fragment := url.Values{}
	fragment.Set("room", room)
	fragment.Set("token", token)
	if strings.TrimSpace(signalURL) != "" {
		fragment.Set("signal", strings.TrimSpace(signalURL))
	}
	if len(iceServers) > 0 {
		encoded, err := json.Marshal(iceServers)
		if err != nil {
			return "", fmt.Errorf("encode viewer ICE configuration: %w", err)
		}
		fragment.Set("ice", string(encoded))
	}
	base.Fragment = ""
	base.RawFragment = ""
	return base.String() + "#" + fragment.Encode(), nil
}

// NewWebSocket connects a host to a room and waits for its welcome message.
func NewWebSocket(ctx context.Context, endpoint, room, token string, maxViewers int) (*WebSocket, error) {
	client := New(endpoint)
	if err := client.connect(ctx, room, token, RoleHost, maxViewers, "", nil); err != nil {
		return nil, err
	}
	return client, nil
}

// NewWebSocketWithIdentity connects a public-lobby host using its signed
// Discord identity token. The Worker verifies the signature before publishing
// the stream in the lobby.
func NewWebSocketWithIdentity(ctx context.Context, endpoint, room, token, identityToken string, maxViewers int, iceServers []ICEServer) (*WebSocket, error) {
	client := New(endpoint)
	if err := client.connect(ctx, room, token, RoleHost, maxViewers, identityToken, iceServers); err != nil {
		return nil, err
	}
	return client, nil
}

// ConnectViewer is primarily useful to command-line diagnostics; browsers
// implement their side independently in viewer/src/transports/webrtc.js.
func ConnectViewer(ctx context.Context, endpoint, room, token string) (*WebSocket, error) {
	client := New(endpoint)
	if err := client.connect(ctx, room, token, RoleViewer, 0, "", nil); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *WebSocket) connect(ctx context.Context, room, token string, role Role, maxViewers int, identityToken string, iceServers []ICEServer) error {
	if c.conn != nil {
		return errors.New("signaling client is already connected")
	}
	if room == "" || token == "" {
		return errors.New("room and token must not be empty")
	}
	wsURL, err := roomWebSocketURL(c.endpoint, room)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, response, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		if response != nil {
			return fmt.Errorf("connect to signaling server (HTTP %s): %w", response.Status, err)
		}
		return fmt.Errorf("connect to signaling server: %w", err)
	}

	if maxViewers < 0 {
		_ = conn.Close()
		return errors.New("max viewers must not be negative")
	}
	hello := Message{
		Version:       ProtocolVersion,
		Type:          "hello",
		Role:          role,
		IdentityToken: identityToken,
		Token:         token,
		MaxViewers:    maxViewers,
		ICE:           iceServers,
		Capabilities:  json.RawMessage(`{"trickleICE":false,"codec":"h264"}`),
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("set signaling handshake deadline: %w", err)
	}
	if err := conn.WriteJSON(hello); err != nil {
		_ = conn.Close()
		return fmt.Errorf("send signaling hello: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("set signaling welcome deadline: %w", err)
	}
	var welcome Message
	if err := conn.ReadJSON(&welcome); err != nil {
		_ = conn.Close()
		return fmt.Errorf("read signaling welcome: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if welcome.Version != ProtocolVersion {
		_ = conn.Close()
		return fmt.Errorf("signaling protocol version %d is unsupported (want %d)", welcome.Version, ProtocolVersion)
	}
	if welcome.Type == "error" {
		_ = conn.Close()
		return fmt.Errorf("signaling rejected connection (%s): %s", welcome.Code, welcome.ErrorMessage)
	}
	if welcome.Type != "welcome" || welcome.PeerID == "" || welcome.Role != role {
		_ = conn.Close()
		return fmt.Errorf("unexpected signaling handshake response %q", welcome.Type)
	}
	c.conn, c.room, c.token, c.role, c.peerID = conn, room, token, role, welcome.PeerID
	c.messages <- welcome
	go c.readLoop()
	go c.heartbeat()
	return nil
}

func roomWebSocketURL(endpoint, room string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("parse signaling.url: %w", err)
	}
	if (base.Scheme != "wss" && base.Scheme != "ws") || base.Host == "" {
		return "", errors.New("signaling.url must be an absolute ws or wss URL")
	}
	base.Path = path.Join(base.Path, "v1", "room", room, "ws")
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

// Connect opens the host WebSocket and queues its welcome message before
// asynchronous events arrive.
func (c *WebSocket) Connect(ctx context.Context, room, token string, maxViewers int) error {
	return c.connect(ctx, room, token, RoleHost, maxViewers, "", nil)
}

// Messages yields protocol messages until the room connection closes.
func (c *WebSocket) Messages() <-chan Message { return c.messages }

// Send writes one protocol message with a bounded deadline.
func (c *WebSocket) Send(ctx context.Context, message Message) error {
	if c.conn == nil {
		return errors.New("signaling client is not connected")
	}
	if message.Version == 0 {
		message.Version = ProtocolVersion
	}
	if message.Version != ProtocolVersion {
		return fmt.Errorf("cannot send protocol version %d (client supports %d)", message.Version, ProtocolVersion)
	}
	select {
	case <-c.done:
		if c.readErr != nil {
			return fmt.Errorf("signaling connection is closed: %w", c.readErr)
		}
		return errors.New("signaling connection is closed")
	default:
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set signaling write deadline: %w", err)
		}
	} else if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set signaling write deadline: %w", err)
	}
	if err := c.conn.WriteJSON(message); err != nil {
		return fmt.Errorf("send signaling %s: %w", message.Type, err)
	}
	return nil
}

// Room returns the room ID.
func (c *WebSocket) Room() string { return c.room }

// Token returns the session token.
func (c *WebSocket) Token() string { return c.token }

// PeerID returns the ID assigned by the signaling server.
func (c *WebSocket) PeerID() string { return c.peerID }

// Err reports why the receive loop exited, after Messages has been closed.
func (c *WebSocket) Err() error {
	select {
	case <-c.done:
		return c.readErr
	default:
		return nil
	}
}

// Close sends bye and closes the socket. It is safe to call more than once.
func (c *WebSocket) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.Send(ctx, Message{Type: "bye", Reason: "host stopped"}); err != nil && !errors.Is(err, websocket.ErrCloseSent) {
			closeErr = err
		}
		if err := c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "leaving"),
			time.Now().Add(time.Second),
		); err != nil && closeErr == nil {
			closeErr = err
		}
		close(c.stop)
		_ = c.conn.Close()
		<-c.done
	})
	return closeErr
}

func (c *WebSocket) readLoop() {
	defer close(c.done)
	defer close(c.messages)
	for {
		var message Message
		if err := c.conn.ReadJSON(&message); err != nil {
			c.readErr = err
			return
		}
		if message.Version != ProtocolVersion {
			c.readErr = fmt.Errorf("signaling server sent unsupported protocol version %d", message.Version)
			return
		}
		if message.Type == "error" && message.Fatal != nil && *message.Fatal {
			c.readErr = fmt.Errorf("signaling error (%s): %s", message.Code, message.ErrorMessage)
		} else if message.Type == "error" {
			c.readErr = fmt.Errorf("signaling error (%s): %s", message.Code, message.ErrorMessage)
		}
		if message.Type == "ping" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.Send(ctx, Message{Type: "pong", Time: message.Time})
			cancel()
			if err != nil {
				c.readErr = fmt.Errorf("reply to signaling ping: %w", err)
				return
			}
		}
		select {
		case c.messages <- message:
		case <-c.stop:
			return
		}
		if message.Type == "error" && (message.Fatal == nil || *message.Fatal) {
			return
		}
	}
}

func (c *WebSocket) heartbeat() {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.Send(ctx, Message{Type: "ping", Time: time.Now().UnixMilli()})
			cancel()
			if err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}
