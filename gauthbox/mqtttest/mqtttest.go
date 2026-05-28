package mqtttest

import (
	"bufio"
	"log/slog"
	"net"
	"sync"
	"testing"
)

// Mock packet types matching MQTT protocol
const (
	mqttPacketConnect    = 1
	mqttPacketConnack    = 2
	mqttPacketPublish    = 3
	mqttPacketSubscribe  = 8
	mqttPacketSuback     = 9
	mqttPacketPingreq    = 12
	mqttPacketPingresp   = 13
)

type MQTTMsg struct {
	Topic   string
	Payload string
}

type MockMQTTServer struct {
	Addr     string
	listener net.Listener
	conns    []net.Conn
	Packets  chan byte
	PubChan  chan MQTTMsg
	done     chan struct{}
	mu       sync.Mutex
}

func StartMockMQTT(t *testing.T) *MockMQTTServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start mock MQTT broker: %v", err)
	}

	server := &MockMQTTServer{
		Addr:     l.Addr().String(),
		listener: l,
		Packets:  make(chan byte, 1000),
		PubChan:  make(chan MQTTMsg, 1000),
		done:     make(chan struct{}),
	}

	go server.acceptConnections()
	return server
}

func (s *MockMQTTServer) acceptConnections() {
	for {
		c, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				slog.Error("Mock MQTT accept error", slog.Any("error", err))
				return
			}
		}

		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()

		go s.handle(c)
	}
}

func (s *MockMQTTServer) handle(c net.Conn) {
	defer c.Close()
	reader := bufio.NewReader(c)
	var data []byte

	for {
		buf := make([]byte, 1024)
		n, err := reader.Read(buf)
		if err != nil {
			return
		}
		data = append(data, buf[:n]...)

		for len(data) > 0 {
			packetType := data[0] >> 4
			select {
			case s.Packets <- packetType:
			default:
			}

			// Variable length remaining length decoding
			remLen := 0
			multiplier := 1
			headerLen := 1
			remLenParsed := false

			for i := 1; i < len(data); i++ {
				headerLen++
				digit := data[i]
				remLen += int(digit&127) * multiplier
				if (digit & 128) == 0 {
					remLenParsed = true
					break
				}
				multiplier *= 128
				if multiplier > 128*128*128 {
					break
				}
			}

			if !remLenParsed {
				break
			}

			totalPacketSize := headerLen + remLen
			if len(data) < totalPacketSize {
				break
			}

			packetData := data[headerLen:totalPacketSize]

			switch packetType {
			case mqttPacketConnect: // CONNECT
				c.Write([]byte{0x20, 0x02, 0x00, 0x00}) // CONNACK
			case mqttPacketPublish: // PUBLISH
				if len(packetData) >= 2 {
					topicLen := int(packetData[0])<<8 | int(packetData[1])
					if len(packetData) >= 2+topicLen {
						topic := string(packetData[2 : 2+topicLen])
						payload := string(packetData[2+topicLen:])
						select {
						case s.PubChan <- MQTTMsg{Topic: topic, Payload: payload}:
						default:
						}
					}
				}
			case mqttPacketSubscribe: // SUBSCRIBE
				if len(packetData) >= 4 {
					id1, id2 := packetData[2], packetData[3]
					c.Write([]byte{0x90, 0x03, id1, id2, 0x00}) // SUBACK
				}
			case mqttPacketPingreq: // PINGREQ
				c.Write([]byte{0xD0, 0x00}) // PINGRESP
			}

			data = data[totalPacketSize:]
		}
	}
}

func (s *MockMQTTServer) Close() {
	close(s.done)
	s.listener.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

func (s *MockMQTTServer) PublishToClient(topic, payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	topicBytes := []byte(topic)
	payloadBytes := []byte(payload)
	topicLen := len(topicBytes)

	remLen := 2 + topicLen + len(payloadBytes)
	packet := []byte{0x30}

	val := remLen
	for {
		digit := byte(val & 127)
		val >>= 7
		if val > 0 {
			digit |= 128
		}
		packet = append(packet, digit)
		if val == 0 {
			break
		}
	}

	packet = append(packet, byte(topicLen>>8), byte(topicLen&255))
	packet = append(packet, topicBytes...)
	packet = append(packet, payloadBytes...)

	for _, c := range s.conns {
		c.Write(packet)
	}
}
