package store

import (
	"csms-engine/internal/dlb"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ChargerState โครงสร้างเก็บสถานะตู้ชาร์จและ WebSocket Connection ใน RAM
type ChargerState struct {
	SerialNumber string                 `json:"serial_number"`
	Conn         *websocket.Conn        `json:"-"`
	Connectors   map[int]dlb.EVSEStatus `json:"connectors"` // เช่น { 1: "Charging", 2: "Available" }
	LastSeen     time.Time              `json:"last_seen"`
}

// DLBExecutionResult รายงานผลการยิง SetChargingProfile รายหัวชาร์จ
type DLBExecutionResult struct {
	SerialNumber  string  `json:"serialNumber"`
	ConnectorID   int     `json:"connectorId"`
	Status        string  `json:"status"`
	AllocatedAmps float64 `json:"allocatedAmps"`
	SentSuccess   bool    `json:"sentSuccess"`
	Note          string  `json:"note,omitempty"`
}

// MemoryStore ตัวจัดการ Data ใน RAM ทั้งหมด (Box State และ Charger Socket State)
type MemoryStore struct {
	boxes          map[string]dlb.ENBox       // key: address_esp32
	boxMaxAmps     map[string]float64         // key: address_esp32 (เก็บ MaxAmp ของแต่ละ Box)
	chargerSockets map[string]*ChargerState   // key: serialNumber
	mu             sync.RWMutex
}

// NewMemoryStore สร้าง Instance สำหรับใช้งาน RAM Store
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		boxes:          make(map[string]dlb.ENBox),
		boxMaxAmps:     make(map[string]float64),
		chargerSockets: make(map[string]*ChargerState),
	}
}

// ==========================================
// 1. WebSocket Manager (Register / Remove)
// ==========================================

// RegisterChargerSocket สั่งลงทะเบียนสาย WebSocket
func (m *MemoryStore) RegisterChargerSocket(serialNumber string, conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// หากมีสายเก่าค้างอยู่ ให้สั่งปิดอย่างปลอดภัยใน 500ms
	if existing, exists := m.chargerSockets[serialNumber]; exists && existing.Conn != nil && existing.Conn != conn {
		oldConn := existing.Conn
		go func() {
			time.Sleep(500 * time.Millisecond)
			oldConn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Replaced by new connection"),
				time.Now().Add(time.Second),
			)
			oldConn.Close()
		}()
	}

	existingConnectors := make(map[int]dlb.EVSEStatus)
	if existing, exists := m.chargerSockets[serialNumber]; exists && existing.Connectors != nil {
		existingConnectors = existing.Connectors
	}

	m.chargerSockets[serialNumber] = &ChargerState{
		SerialNumber: serialNumber,
		Conn:         conn,
		Connectors:   existingConnectors,
		LastSeen:     time.Now(),
	}
}

// RemoveChargerSocket สั่งลบสายออกเมื่อตู้ตัดการเชื่อมต่อ
func (m *MemoryStore) RemoveChargerSocket(serialNumber string, connToRemove *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if current, exists := m.chargerSockets[serialNumber]; exists {
		if connToRemove == nil || current.Conn == connToRemove {
			delete(m.chargerSockets, serialNumber)
		}
	}
}

// UpdateChargerStatus อัปเดตสถานะหัวชาร์จแต่ละ Connector
func (m *MemoryStore) UpdateChargerStatus(serialNumber string, connectorID int, status dlb.EVSEStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	charger, exists := m.chargerSockets[serialNumber]
	if !exists {
		charger = &ChargerState{
			SerialNumber: serialNumber,
			Connectors:   make(map[int]dlb.EVSEStatus),
		}
	}

	if charger.Connectors == nil {
		charger.Connectors = make(map[int]dlb.EVSEStatus)
	}

	charger.Connectors[connectorID] = status
	charger.LastSeen = time.Now()
	m.chargerSockets[serialNumber] = charger
}

// GetChargerSocket ดึง Connection ของตู้ชาร์จออกไปใช้งาน
func (m *MemoryStore) GetChargerSocket(serialNumber string) (*websocket.Conn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	charger, exists := m.chargerSockets[serialNumber]
	if !exists || charger == nil {
		return nil, false
	}
	return charger.Conn, true
}

// GetAllEVSEs ดึงหัวชาร์จทั้งหมดไปคำนวณ DLB
func (m *MemoryStore) GetAllEVSEs() map[string]*dlb.EVSE {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*dlb.EVSE)
	for sn, charger := range m.chargerSockets {
		for connID, status := range charger.Connectors {
			key := fmt.Sprintf("%s_%d", sn, connID)
			result[key] = &dlb.EVSE{
				SerialNumber: sn,
				ConnectorID:  connID,
				Status:       status,
			}
		}
	}
	return result
}

// ==========================================
// 2. Box Management & DLB Execution
// ==========================================

// SaveBoxToRam บันทึกข้อมูล ControllerBox
func (m *MemoryStore) SaveBoxToRam(box dlb.ENBox, maxAmp float64) dlb.ENBox {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.boxes[box.AddressEsp32] = box
	m.boxMaxAmps[box.AddressEsp32] = maxAmp
	return box
}

// SendSetChargingProfile ยิงคำสั่ง OCPP SetChargingProfile (JSON-RPC) ไปยังตู้ชาร์จ
func (m *MemoryStore) SendSetChargingProfile(serialNumber string, connectorID int, limitAmps float64) bool {
	m.mu.RLock()
	charger, exists := m.chargerSockets[serialNumber]
	m.mu.RUnlock()

	if !exists || charger == nil || charger.Conn == nil {
		return false
	}

	// สร้าง Payload รูปแบบ JSON-RPC Frame ตาม OCPP 1.6 Spec
	msgID := fmt.Sprintf("msg-%d", time.Now().UnixNano())
	payload := []interface{}{
		2,
		msgID,
		"SetChargingProfile",
		map[string]interface{}{
			"connectorId": connectorID,
			"csChargingProfiles": map[string]interface{}{
				"chargingProfileId":      connectorID,
				"stackLevel":             1,
				"chargingProfilePurpose": "TxDefaultProfile",
				"chargingProfileKind":    "Absolute",
				"chargingSchedule": map[string]interface{}{
					"chargingRateUnit": "A",
					"chargingSchedulePeriod": []map[string]interface{}{
						{"startPeriod": 0, "limit": limitAmps},
					},
				},
			},
		},
	}

	// 🎯 แก้ไข: ไม่ใช้ m.mu.Lock() ครอบ WriteJSON ป้องกัน Deadlock และ Blocking
	_ = charger.Conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	err := charger.Conn.WriteJSON(payload)
	_ = charger.Conn.SetWriteDeadline(time.Time{})

	return err == nil
}

// UpdateNowAmpAndSendWs ประมวลผล DLB และสั่งจ่ายไฟไปยังตู้
func (m *MemoryStore) UpdateNowAmpAndSendWs(addressEsp32 string, newNowAmp float64, buildingCap dlb.BuildingCapacity) (map[string]interface{}, error) {
	m.mu.Lock()
	box, exists := m.boxes[addressEsp32]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("ไม่พบข้อมูล ControllerBox: %s", addressEsp32)
	}

	box.NowAmp = newNowAmp
	m.boxes[addressEsp32] = box
	boxMaxAmp := m.boxMaxAmps[addressEsp32]
	m.mu.Unlock()

	// 1. รวบรวม EVSE Data ปัจจุบันส่งให้ DLB Algorithm คำนวณ
	m.mu.RLock()
	evsesMap := make(map[string]*dlb.EVSE)
	for _, sn := range box.IDEVSE {
		if charger, found := m.chargerSockets[sn]; found {
			for connID, status := range charger.Connectors {
				key := fmt.Sprintf("%s_%d", sn, connID)
				evsesMap[key] = &dlb.EVSE{
					SerialNumber: sn,
					ConnectorID:  connID,
					Status:       status,
					MaxAmperes:   boxMaxAmp,
				}
			}
		}
	}
	m.mu.RUnlock()

	// 2. เรียกใช้ Pure Logic DLB Algorithm
	dlbRes := dlb.CalculateDLB(buildingCap, box, evsesMap)

	// 3. 🎯 คัดลอกเป้าหมายที่จะยิงออกมาก่อนภายใต้ RLock
	type chargerTarget struct {
		sn     string
		connID int
		status dlb.EVSEStatus
	}

	var targets []chargerTarget

	m.mu.RLock()
	for _, sn := range box.IDEVSE {
		if charger, found := m.chargerSockets[sn]; found {
			for connID, status := range charger.Connectors {
				targets = append(targets, chargerTarget{
					sn:     sn,
					connID: connID,
					status: status,
				})
			}
		}
	}
	m.mu.RUnlock() // 🔓 ปลดล็อก MemoryStore ทันที ก่อนเริ่มยิง WebSocket เพื่อแก้ Deadlock

	// 4. วนลูปยิงคำสั่ง SetChargingProfile นอก Lock
	var results []DLBExecutionResult
	activeCount := 0

	for _, t := range targets {
		key := fmt.Sprintf("%s_%d", t.sn, t.connID)
		allocated, evaluated := dlbRes.Allocations[key]

		if t.status == dlb.StatusCharging {
			activeCount++
			isSent := false
			if evaluated && allocated > 0 {
				isSent = m.SendSetChargingProfile(t.sn, t.connID, allocated)
			}
			results = append(results, DLBExecutionResult{
				SerialNumber:  t.sn,
				ConnectorID:   t.connID,
				Status:        string(t.status),
				AllocatedAmps: allocated,
				SentSuccess:   isSent,
			})
		} else {
			results = append(results, DLBExecutionResult{
				SerialNumber:  t.sn,
				ConnectorID:   t.connID,
				Status:        string(t.status),
				AllocatedAmps: 0,
				SentSuccess:   false,
				Note:          "ไม่ได้อยู่ในสถานะกำลังชาร์จ (Skip)",
			})
		}
	}

	message := "เกลี่ยไฟสำเร็จ"
	if dlbRes.TotalAllocated == 0 && activeCount > 0 {
		message = "ไฟไม่พอ สั่ง Pause (0A)"
	} else if activeCount == 0 {
		message = "ไม่มีรถกำลังชาร์จ"
	}

	return map[string]interface{}{
		"message":                   message,
		"activeConnectorCount":      activeCount,
		"allocatedAmpsPerConnector": dlbRes.TotalAllocated,
		"results":                   results,
	}, nil
}

// GetAllFromRam ดึงข้อมูลใน RAM ทั้งหมด
func (m *MemoryStore) GetAllFromRam() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []map[string]interface{}

	for espAddr, box := range m.boxes {
		var chargersInfo []map[string]interface{}

		for _, sn := range box.IDEVSE {
			if charger, exists := m.chargerSockets[sn]; exists {
				isOnline := charger.Conn != nil
				chargersInfo = append(chargersInfo, map[string]interface{}{
					"serialNumber": sn,
					"isOnline":     isOnline,
					"lastSeen":     charger.LastSeen,
					"connectors":   charger.Connectors,
				})
			} else {
				chargersInfo = append(chargersInfo, map[string]interface{}{
					"serialNumber": sn,
					"isOnline":     false,
					"connectors":   map[int]dlb.EVSEStatus{},
				})
			}
		}

		result = append(result, map[string]interface{}{
			"address_esp32": espAddr,
			"idevse":        box.IDEVSE,
			"NowAmp":        box.NowAmp,
			"MaxAmp":        m.boxMaxAmps[espAddr],
			"chargers":      chargersInfo,
		})
	}

	return result
}