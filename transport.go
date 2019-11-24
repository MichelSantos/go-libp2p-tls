package libp2ptls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	ci "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/sec"
)

// TLS 1.3 is opt-in in Go 1.12
// Activate it by setting the tls13 GODEBUG flag.
func init() {
	os.Setenv("GODEBUG", os.Getenv("GODEBUG")+",tls13=1")
}

// ID is the protocol ID (used when negotiating with multistream)
const ID = "/tls/1.0.0"

const errMessageSimultaneousConnect = "tls: received unexpected handshake message of type *tls.clientHelloMsg when waiting for *tls.serverHelloMsg"

// Transport constructs secure communication sessions for a peer.
type Transport struct {
	identity *Identity

	localPeer peer.ID
	privKey   ci.PrivKey
}

// New creates a TLS encrypted transport
func New(key ci.PrivKey) (*Transport, error) {
	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return nil, err
	}
	t := &Transport{
		localPeer: id,
		privKey:   key,
	}

	identity, err := NewIdentity(key)
	if err != nil {
		return nil, err
	}
	t.identity = identity
	return t, nil
}

var _ sec.SecureTransport = &Transport{}

// SecureInbound runs the TLS handshake as a server.
func (t *Transport) SecureInbound(ctx context.Context, insecure net.Conn) (sec.SecureConn, error) {
	config, keyCh := t.identity.ConfigForAny()
	return t.handshake(ctx, tls.Server(insecure, config), keyCh)
}

// SecureOutbound runs the TLS handshake as a client.
// Note that SecureOutbound will not return an error if the server doesn't
// accept the certificate. This is due to the fact that in TLS 1.3, the client
// sends its certificate and the ClientFinished in the same flight, and can send
// application data immediately afterwards.
// If the handshake fails, the server will close the connection. The client will
// notice this after 1 RTT when calling Read.
func (t *Transport) SecureOutbound(ctx context.Context, insecure net.Conn, p peer.ID) (sec.SecureConn, error) {
	config, keyCh := t.identity.ConfigForPeer(p)
	conn, err := t.handshake(ctx, tls.Client(insecure, config), keyCh)
	if err != nil && err.Error() == errMessageSimultaneousConnect {
		// catch the TLS alert that's still in flight
		config, _ = t.identity.ConfigForAny()
		fmt.Println(p, "waiting for alert")
		err := tls.Server(insecure, config).Handshake()
		if err == nil || err.Error() != "remote error: tls: unexpected message" {
			fmt.Println(err)
			return nil, errors.New("didn't receive expected TLS alert")
		}
		fmt.Println(p, "received alert")
		// Now start the next connection attempt.
		switch comparePeerIDs(t.localPeer, p) {
		case 0:
			return nil, errors.New("tried to simultaneous connect to oneself")
		case -1:
			fmt.Println(p, "Retrying as a client")
			// SHA256(our peer ID) is smaller than SHA256(their peer ID).
			// We're the client in the next connection attempt.
			config, keyCh := t.identity.ConfigForPeer(p)
			return t.handshake(ctx, tls.Client(insecure, config), keyCh)
		case 1:
			fmt.Println(p, "Retrying as a server")
			// SHA256(our peer ID) is larger than SHA256(their peer ID).
			// We're the server in the next connection attempt.
			config, keyCh := t.identity.ConfigForPeer(p)
			return t.handshake(ctx, tls.Server(insecure, config), keyCh)
		default:
			panic("unexpected peer ID comparison result")
		}
	}
	return conn, err
}

func (t *Transport) handshake(
	ctx context.Context,
	tlsConn *tls.Conn,
	keyCh <-chan ci.PubKey,
) (sec.SecureConn, error) {
	// There's no way to pass a context to tls.Conn.Handshake().
	// See https://github.com/golang/go/issues/18482.
	// Close the connection instead.
	select {
	case <-ctx.Done():
		tlsConn.Close()
	default:
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Ensure that we do not return before
	// either being done or having a context
	// cancellation.
	defer wg.Wait()
	defer close(done)

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-done:
		case <-ctx.Done():
			tlsConn.Close()
		}
	}()

	if err := tlsConn.Handshake(); err != nil {
		// if the context was canceled, return the context error
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	// Should be ready by this point, don't block.
	var remotePubKey ci.PubKey
	select {
	case remotePubKey = <-keyCh:
	default:
	}
	if remotePubKey == nil {
		return nil, errors.New("go-libp2p-tls BUG: expected remote pub key to be set")
	}

	conn, err := t.setupConn(tlsConn, remotePubKey)
	if err != nil {
		// if the context was canceled, return the context error
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return conn, nil
}

func (t *Transport) setupConn(tlsConn *tls.Conn, remotePubKey ci.PubKey) (sec.SecureConn, error) {
	remotePeerID, err := peer.IDFromPublicKey(remotePubKey)
	if err != nil {
		return nil, err
	}
	return &conn{
		Conn:         tlsConn,
		localPeer:    t.localPeer,
		privKey:      t.privKey,
		remotePeer:   remotePeerID,
		remotePubKey: remotePubKey,
	}, nil
}
