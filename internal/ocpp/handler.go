package v16

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"csms-engine/internal/dlb"
	"csms-engine/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// 🎯 ตั้งเวลารอรับข้อความ/Heartbeat จากตู้ (เช่น 10 นาที)
const readWait = 10 * time.Minute

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{"ocpp1.6", "ocpp2.0.1"},
}

// 🎯 ระบบจัดการ Write Mutex กลางแยกราย Connection (Thread-Safe)
var (
	connMutexes     = make(map[*websocket.Conn]*sync.Mutex)
	connMutexesLock sync.Mutex
)

func getConnMutex(conn *websocket.Conn) *sync.Mutex {
	connMutexesLock.Lock()
	defer connMutexesLock.Unlock()
	mu, exists := connMutexes[conn]
	if !exists {
		mu = &sync.Mutex{}
		connMutexes[conn] = mu
	}
	return mu
}

func removeConnMutex(conn *websocket.Conn) {
	connMutexesLock.Lock()
	defer connMutexesLock.Unlock()
	delete(connMutexes, conn)
}

// 🎯 SafeWriteJSON (S ตัวใหญ่) ให้ไฟล์อื่น เช่น BoxHandler เรียกยิง WebSocket ได้ปลอดภัย ไม่ติด Block
func SafeWriteJSON(conn *websocket.Conn, v interface{}) error {
	mu := getConnMutex(conn)
	mu.Lock()
	defer mu.Unlock()

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := conn.WriteJSON(v)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

type OCPPHandler struct {
	store *store.MemoryStore
}

func NewOCPPHandler(memStore *store.MemoryStore) *OCPPHandler {
	return &OCPPHandler{store: memStore}
}

func (h *OCPPHandler) HandleWS(c *gin.Context) {
	serialNumber := c.Param("serialNumber")

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("❌ [WS Upgrade Failed] ตู้ S/N: %s Error: %v", serialNumber, err)
		return
	}

	// 🎯 1. ตั้ง ReadLimit และขยาย ReadDeadline เป็น 10 นาที
	conn.SetReadLimit(65536)
	_ = conn.SetReadDeadline(time.Now().Add(readWait))

	// 🎯 2. ดักรับ WS Ping จากฝั่ง Simulator/ตู้ชาร์จ
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		mu := getConnMutex(conn)
		mu.Lock()
		defer mu.Unlock()

		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
		_ = conn.SetWriteDeadline(time.Time{})
		return err
	})

	// 🎯 3. ดักรับ WS Pong จากตู้
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		return nil
	})

	h.store.RegisterChargerSocket(serialNumber, conn)
	log.Printf("🔌 [OCPP 1.6 Connected] ตู้ S/N: %s ต่อออนไลน์สำเร็จ (Protocol: %s)", serialNumber, conn.Subprotocol())

	defer func() {
		h.store.RemoveChargerSocket(serialNumber, conn)
		removeConnMutex(conn)
		conn.Close()
		log.Printf("🔌 [OCPP Disconnected] ตู้ S/N: %s หลุดจากการเชื่อมต่อ", serialNumber)
	}()

	// 4. Read Loop รอรับ OCPP Message
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("⚠️ [WS Read Error] ตู้ S/N: %s Reason: %v", serialNumber, err)
			break
		}

		_ = conn.SetReadDeadline(time.Now().Add(readWait))

		if messageType == websocket.TextMessage {
			log.Printf("📨 [OCPP Raw Msg %s]: %s", serialNumber, string(message))
			h.processOCPPMessage(serialNumber, conn, message)
		}
	}
}

func (h *OCPPHandler) processOCPPMessage(serialNumber string, conn *websocket.Conn, rawMsg []byte) {
	var rawFrame []json.RawMessage
	if err := json.Unmarshal(rawMsg, &rawFrame); err != nil || len(rawFrame) < 3 {
		log.Printf("⚠️ [Invalid JSON-RPC Format] ตู้ S/N: %s", serialNumber)
		return
	}

	var messageTypeID int
	var messageID string
	var action string

	_ = json.Unmarshal(rawFrame[0], &messageTypeID)
	_ = json.Unmarshal(rawFrame[1], &messageID)

	if messageTypeID == 2 {
		_ = json.Unmarshal(rawFrame[2], &action)

		var payload map[string]interface{}
		if len(rawFrame) > 3 {
			_ = json.Unmarshal(rawFrame[3], &payload)
		}

		switch action {
		case "BootNotification":
			h.handleBootNotification(serialNumber, conn, messageID, payload)
		case "Heartbeat":
			h.handleHeartbeat(serialNumber, conn, messageID)
		case "StatusNotification":
			h.handleStatusNotification(serialNumber, conn, messageID, payload)
		default:
			log.Printf("ℹ️ [Unhandled Action] ตู้ S/N: %s Action: %s", serialNumber, action)
		}
	}
}

func (h *OCPPHandler) handleBootNotification(serialNumber string, conn *websocket.Conn, msgID string, payload map[string]interface{}) {
	log.Printf("📌 [BootNotification] ตู้ S/N: %s (Vendor: %v, Model: %v)", serialNumber, payload["chargePointVendor"], payload["chargePointModel"])

	response := []interface{}{
		3,
		msgID,
		map[string]interface{}{
			"status":      "Accepted",
			"currentTime": time.Now().Format(time.RFC3339),
			"interval":    60,
		},
	}

	_ = SafeWriteJSON(conn, response)
}

func (h *OCPPHandler) handleHeartbeat(serialNumber string, conn *websocket.Conn, msgID string) {
	log.Printf("💓 [Heartbeat] ตู้ S/N: %s", serialNumber)

	response := []interface{}{
		3,
		msgID,
		map[string]interface{}{
			"currentTime": time.Now().Format(time.RFC3339),
		},
	}

	_ = SafeWriteJSON(conn, response)
}

func (h *OCPPHandler) handleStatusNotification(serialNumber string, conn *websocket.Conn, msgID string, payload map[string]interface{}) {
	connectorIDFloat, _ := payload["connectorId"].(float64)
	connectorID := int(connectorIDFloat)
	rawStatus, _ := payload["status"].(string)

	log.Printf("🔌 [StatusNotification] ตู้ S/N: %s Connector: %d Status: %s", serialNumber, connectorID, rawStatus)

	var status dlb.EVSEStatus
	switch strings.ToUpper(rawStatus) {
	case "CHARGING":
		status = dlb.StatusCharging
	case "PREPARING":
		status = dlb.StatusPreparing
	case "SUSPENDEDEV", "SUSPENDEDEVSE":
		status = dlb.StatusSuspendedEV
	case "FAULTED":
		status = dlb.StatusFaulted
	case "UNAVAILABLE":
		status = dlb.StatusOffline
	default:
		status = dlb.StatusAvailable
	}

	h.store.UpdateChargerStatus(serialNumber, connectorID, status)

	response := []interface{}{
		3,
		msgID,
		map[string]interface{}{},
	}

	_ = SafeWriteJSON(conn, response)
}