package dlb

import (
	"fmt"
	"math"
)

// CalculateDLB คำนวณการเกลี่ยไฟ Dynamic Load Balancing ตามกติกาจาก BoxService
// - building: พิกัดกระแสไฟรวมของอาคาร (เปรียบเทียบกับ NowAmp)
// - boxes: รายการกล่อง ENBox (ESP32) ที่ดูแลตู้/หัวชาร์จ
// - evses: Map ของหัวชาร์จทั้งหมดที่มีในระบบ (Key: "SN_ConnectorID" เช่น "CS-SIEMENS_1")
func CalculateDLB(building BuildingCapacity, box ENBox, evses map[string]*EVSE) DLBResult {
	fmt.Printf("%v\n", evses)

	// 🎯 เก็บทั้ง key และ evse คู่กัน (เดิมเก็บแค่ *EVSE เฉยๆ ทำให้ตอน redistribute ไม่รู้ key เต็ม)
	type target struct {
		key  string
		evse *EVSE
	}
	var activeTargets []target

	fmt.Printf("🔹 [DLB] คำนวณ DLB สำหรับ Box: %s, NowAmp: %.2fA, Building MaxAmp: %.2fA\n", box.AddressEsp32, box.NowAmp, building.MaxAmperes)

	// 1. ค้นหาหัวชาร์จเป้าหมายที่อยู่ในสถานะ Charging
	for key, evse := range evses {
		isMyBox := false
		for _, idEvse := range box.IDEVSE {
			if evse.SerialNumber == idEvse.Name || key == idEvse.Name {
				isMyBox = true
				break
			}
		}

		if isMyBox && evse.Status == StatusCharging {
			activeTargets = append(activeTargets, target{key: key, evse: evse})
		}
	}

	activeCount := len(activeTargets)
	allocations := make(map[string]float64)

	fmt.Printf("🔹 [DLB] จำนวนหัวชาร์จที่กำลังชาร์จ: %d activeTargets: %v\n", activeCount, activeTargets)

	if activeCount == 0 {
		return DLBResult{
			Allocations:    allocations,
			TotalAllocated: 0,
		}
	}

	availableAmp := (building.MaxAmperes * 0.8) - box.NowAmp
	if availableAmp < 0 {
		availableAmp = 0
	}

	fmt.Printf("🔹 [DLB] ไฟคงเหลือสำหรับ EV ทั้งหมด: %.2fA | จำนวนหัวที่ชาร์จ: %d\n", availableAmp, activeCount)

	// 🎯 ==========================================
	// 🎯 Water-Filling: เกลี่ยไฟแบบวนซ้ำ
	// 🎯 หัวไหนติด MaxAmp (รับไม่หมดตามส่วนแบ่งเฉลี่ย) จะถูกตรึงไว้ที่ MaxAmp ของตัวเอง
	// 🎯 แล้วเอาไฟส่วนที่เหลือ (ไม่ได้ใช้) ไปหารเฉลี่ยใหม่ให้เฉพาะหัวที่ยังไม่ถูกตรึง
	// 🎯 วนจนกว่าจะไม่มีใครติด cap อีก หรือไฟหมด
	// 🎯 ==========================================
	finalAmps := make(map[string]float64) // key -> แอมป์ที่ตรึงแล้ว (final)
	pending := activeTargets              // หัวที่ยังไม่ถูกตรึง
	remainingAmp := availableAmp

	for len(pending) > 0 {
		share := math.Floor(remainingAmp / float64(len(pending)))

		var stillPending []target
		cappedAny := false

		for _, t := range pending {
			maxAmp := t.evse.MaxAmperes
			if maxAmp > 0 && share > maxAmp {
				// หัวนี้รับได้ไม่ถึงส่วนแบ่งเฉลี่ย -> ตรึงไว้ที่ MaxAmp ของมัน
				finalAmps[t.key] = maxAmp
				remainingAmp -= maxAmp
				cappedAny = true
			} else {
				stillPending = append(stillPending, t)
			}
		}

		if remainingAmp < 0 {
			remainingAmp = 0
		}

		if !cappedAny {
			// ไม่มีใครติด cap แล้ว -> แจก share เท่ากันให้ทุกหัวที่เหลือ แล้วจบ
			for _, t := range stillPending {
				finalAmps[t.key] = share
			}
			break
		}

		pending = stillPending
		if len(pending) == 0 {
			break
		}
	}

	fmt.Printf("🔹 [DLB] ผลเกลี่ยไฟหลัง Water-Filling: %v\n", finalAmps)

	var totalAllocated float64

	// 2. กติกาที่ 2: ถ้าไฟที่ได้จริงต่ำกว่า 6A -> ปรับเป็น 0A (สั่ง Pause)
	// 🎯 ใช้ threshold 6A ต่อ "หัว" แทนการเช็คแค่ค่าเฉลี่ยกลางตัวเดียวแบบเดิม
	//    เพราะตอนนี้แต่ละหัวได้ไฟไม่เท่ากันแล้วจากการ redistribute
	for key, amp := range finalAmps {
		if amp < 6.0 {
			finalAmps[key] = 0.0
		}
	}

	// 3. เขียนผลลัพธ์ลง allocations (คงรูปแบบเดิม: เก็บทั้ง key เต็มและ key แบบสั้น)
	for key, evse := range evses {
		isMyBox := false
		for _, idEvse := range box.IDEVSE {
			if evse.SerialNumber == idEvse.Name || key == idEvse.Name {
				isMyBox = true
				break
			}
		}

		if !isMyBox {
			continue
		}

		if evse.Status == StatusCharging {
			targetAmp := finalAmps[key] // ค่าที่ได้จริงหลัง water-filling + threshold

			allocations[key] = targetAmp
			allocations[evse.SerialNumber] = targetAmp

			totalAllocated += targetAmp
		} else {
			allocations[key] = 0.0
			allocations[evse.SerialNumber] = 0.0
		}
	}

	return DLBResult{
		Allocations:    allocations,
		TotalAllocated: totalAllocated,
	}
}