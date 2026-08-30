package dlb

// BuildingCapacity กำหนดพิกัดกระแสไฟรวมสูงสุดของอาคาร
type BuildingCapacity struct {
	MaxAmperes float64 // กระแสไฟสูงสุดที่ตึกสามารถจ่ายให้ระบบชาร์จได้ (Amperes)
}

// EVSEStatus สถานะของหัวชาร์จ
type EVSEStatus string

const (
	StatusAvailable   EVSEStatus = "Available"
	StatusPreparing   EVSEStatus = "Preparing"
	StatusCharging    EVSEStatus = "Charging"
	StatusSuspendedEV EVSEStatus = "SuspendedEV"
	StatusFaulted     EVSEStatus = "Faulted"
	StatusOffline     EVSEStatus = "Offline"
)

// EVSE ข้อมูลหัวชาร์จ (Electric Vehicle Supply Equipment)
type EVSE struct {
	SerialNumber   string     `json:"sn"`
	ConnectorID    int        `json:"connector_id"`
	Status         EVSEStatus `json:"status"`
	CurrentAmperes float64    `json:"current_amperes"` // กระแสไฟที่ใช้อยู่ ณ ปัจจุบัน
	MaxAmperes     float64    `json:"max_amperes"`     // โควตาสูงสุดที่หัวชาร์จรับได้
}

type ENBox struct {
	AddressEsp32         string        `json:"address_esp32"`
	IDEVSE          []string           `json:"idevse"`
	NowAmp			float64            `json:"NowAmp"`
}

// DLBResult ผลลัพธ์การจัดสรรกระแสไฟจากระบบ Dynamic Load Balancing
type DLBResult struct {
	Allocations    map[string]float64 `json:"allocations"`     // ID ตู้/หัวชาร์จ -> จำนวนแอมป์ที่จัดสรรให้
	TotalAllocated float64            `json:"total_allocated"` // รวมแอมป์ที่จัดสรรไปทั้งหมด
}