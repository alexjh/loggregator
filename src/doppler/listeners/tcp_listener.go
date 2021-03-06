package listeners

import (
	"crypto/tls"
	"crypto/x509"
	"doppler/config"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sync"

	"github.com/cloudfoundry/dropsonde/dropsonde_unmarshaller"
	"github.com/cloudfoundry/dropsonde/metrics"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/sonde-go/events"
)

type TCPListener struct {
	envelopeChan   chan *events.Envelope
	logger         *gosteno.Logger
	listener       net.Listener
	connections    map[net.Conn]struct{}
	unmarshaller   dropsonde_unmarshaller.DropsondeUnmarshaller
	stopped        chan struct{}
	lock           sync.Mutex
	listenerClosed chan struct{}
	started        bool

	receivedMessageCountMetricName string
	receivedByteCountMetricName    string
	receiveErrorCountMetricName    string
}

func NewTLSConfig(certFile, keyFile, caCertFile string) (*tls.Config, error) {
	tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load keypair: %s", err.Error())
	}

	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{tlsCert},
		InsecureSkipVerify: false,
		ClientAuth:         tls.RequireAndVerifyClientCert,
		MinVersion:         tls.VersionTLS12,
	}

	if caCertFile != "" {
		certBytes, err := ioutil.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("failed read ca cert file: %s", err.Error())
		}

		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(certBytes); !ok {
			return nil, errors.New("Unable to load caCert")
		}
		tlsConfig.RootCAs = caCertPool
		tlsConfig.ClientCAs = caCertPool
	}

	return tlsConfig, nil
}

func NewTCPListener(contextName string, address string, tlsListenerConfig *config.TLSListenerConfig, envelopeChan chan *events.Envelope, logger *gosteno.Logger) (Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	if tlsListenerConfig != nil {
		tlsConfig, err := NewTLSConfig(tlsListenerConfig.CertFile, tlsListenerConfig.KeyFile, tlsListenerConfig.CAFile)
		if err != nil {
			return nil, err
		}
		listener = tls.NewListener(listener, tlsConfig)
	}

	return &TCPListener{
		listener:       listener,
		envelopeChan:   envelopeChan,
		logger:         logger,
		connections:    make(map[net.Conn]struct{}),
		unmarshaller:   dropsonde_unmarshaller.NewDropsondeUnmarshaller(logger),
		stopped:        make(chan struct{}),
		listenerClosed: make(chan struct{}),

		receivedMessageCountMetricName: contextName + ".receivedMessageCount",
		receivedByteCountMetricName:    contextName + ".receivedByteCount",
		receiveErrorCountMetricName:    contextName + ".receiveErrorCount",
	}, nil
}

func (t *TCPListener) Address() string {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.listener != nil {
		return t.listener.Addr().String()
	}
	return ""
}

func (t *TCPListener) Start() {
	t.lock.Lock()
	if t.started {
		t.lock.Unlock()
		t.logger.Fatal("TCPListener has already been started")
	}
	t.started = true
	listener := t.listener
	t.lock.Unlock()

	t.logger.Infof("TCP listener listening on %s", t.Address())
	for {
		conn, err := listener.Accept()
		if err != nil {
			close(t.listenerClosed)
			t.logger.Debugf("Error while reading: %s", err)
			return
		}
		t.addConnection(conn)
		go t.handleConnection(conn)
	}
}

func (t *TCPListener) Stop() {
	t.lock.Lock()
	if t.listener == nil {
		t.lock.Unlock()
		return
	}

	close(t.stopped)
	t.listener.Close()
	t.listener = nil

	for conn, _ := range t.connections {
		conn.Close()
	}
	done := t.listenerClosed
	t.lock.Unlock()

	<-done
}

func (t *TCPListener) addConnection(conn net.Conn) {
	t.lock.Lock()
	t.connections[conn] = struct{}{}
	t.lock.Unlock()
}

func (t *TCPListener) removeConnection(conn net.Conn) {
	t.lock.Lock()
	delete(t.connections, conn)
	t.lock.Unlock()
}

func (t *TCPListener) handleConnection(conn net.Conn) {
	defer conn.Close()
	defer t.removeConnection(conn)

	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.Handshake(); err != nil {
			t.logger.Warnd(map[string]interface{}{
				"error":   err.Error(),
				"address": conn.RemoteAddr().String(),
			}, "TLS handshake error")
			metrics.BatchIncrementCounter(t.receiveErrorCountMetricName)
			return
		}
	}

	var n uint32
	var bytes []byte
	var err error

	for {
		err = binary.Read(conn, binary.LittleEndian, &n)
		if err != nil {
			if err != io.EOF {
				metrics.BatchIncrementCounter(t.receiveErrorCountMetricName)
				t.logger.Errorf("Error while decoding: %v", err)
			}
			break
		}

		read := bytes
		if cap(bytes) < int(n) {
			bytes = make([]byte, int(n))
		}
		read = bytes[:n]

		_, err = io.ReadFull(conn, read)
		if err != nil {
			metrics.BatchIncrementCounter(t.receiveErrorCountMetricName)
			t.logger.Errorf("Error during i/o read: %v", err)
			break
		}

		envelope, err := t.unmarshaller.UnmarshallMessage(read)
		if err != nil {
			continue
		}
		metrics.BatchIncrementCounter(t.receivedMessageCountMetricName)
		metrics.BatchAddCounter(t.receivedByteCountMetricName, uint64(n+4))

		select {
		case t.envelopeChan <- envelope:
		case <-t.stopped:
			return
		}
	}

}
