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
	var activeTargets []*EVSE

	fmt.Printf("🔹 [DLB] คำนวณ DLB สำหรับ Box: %s, NowAmp: %.2fA, Building MaxAmp: %.2fA\n", box.AddressEsp32, box.NowAmp, building.MaxAmperes)

	// 1. ค้นหาหัวชาร์จเป้าหมายที่อยู่ในสถานะ Charging
	// 🎯 แก้ไข: เปลี่ยนมาวนลูปค้นหาจาก map evses โดยตรง เพื่อให้เจอทุก Connector ของตู้ (ไม่ใช่แค่ _1)
	for key, evse := range evses {
		// เช็คว่าหัวชาร์จนี้เป็นของกล่องนี้หรือไม่
		isMyBox := false
		for _, idEvse := range box.IDEVSE {
			if evse.SerialNumber == idEvse || key == idEvse {
				isMyBox = true
				break
			}
		}

		if isMyBox && evse.Status == StatusCharging {
			activeTargets = append(activeTargets, evse)
		}
	}

	activeCount := len(activeTargets)
	allocations := make(map[string]float64)

	fmt.Printf("🔹 [DLB] จำนวนหัวชาร์จที่กำลังชาร์จ: %d activeTargets: %v\n", activeCount, activeTargets)

	// ถ้าไม่มีรถกำลังชาร์จ คืนค่า 0 ทั้งหมด
	if activeCount == 0 {
		return DLBResult{
			Allocations:    allocations,
			TotalAllocated: 0,
		}
	}

	// 2. คำนวณไฟหารเฉลี่ยตามจำนวนหัวชาร์จที่กำลังทำงาน (Math.floor(newNowAmp / activeCount))
	// ใช้วิธีนำค่าต่ำสุดระหว่าง กระแสไฟอาคาร (building.MaxAmperes) กับ กระแสไฟกล่อง (box.NowAmp)
	availableAmp := (building.MaxAmperes * 0.8) - box.NowAmp // ลดลง 20% เพื่อเผื่อความปลอดภัย
	if availableAmp < 0 {
		availableAmp = 0
	}

	allocatedAmps := math.Floor(availableAmp / float64(activeCount))

	fmt.Printf("🔹 [DLB] ไฟคงเหลือสำหรับ EV: %.2fA | จำนวนหัวที่ชาร์จ: %d | เกลี่ยได้หัวละ: %.2fA\n", availableAmp, activeCount, allocatedAmps)

	// 3. กติกาที่ 1: ถ้าไฟหารได้เกินกว่า MaxAmperes ที่ EVSE/Box รับได้ -> ปรับลงเท่า MaxAmperes
	// (ใช้ MaxAmperes ของหัวชาร์จแต่ละหัวในการ cap ขีดจำกัด)

	// 4. กติกาที่ 2: ถ้าไฟหารได้ต่ำกว่า 6A -> ปรับเป็น 0A (สั่ง Pause)
	if allocatedAmps < 6.0 {
		allocatedAmps = 0.0
	}

	var totalAllocated float64

	// 5. จัดสรรกระแสไฟให้หัวชาร์จแต่ละจุด
	// 🎯 แก้ไข: วนลูปจาก evses เพื่อบันทึกค่าแยกราย Connector เช่น "CS-SIEMENS_1", "CS-SIEMENS_2" จะได้ไม่ทับกัน
	for key, evse := range evses {
		isMyBox := false
		for _, idEvse := range box.IDEVSE {
			if evse.SerialNumber == idEvse || key == idEvse {
				isMyBox = true
				break
			}
		}

		// ถ้าไม่ใช่ตู้ของกล่องนี้ ให้ข้ามไป
		if !isMyBox {
			continue
		}

		if evse.Status == StatusCharging {
			targetAmp := allocatedAmps

			// เช็กไม่ให้เกิน MaxAmperes ของหัวชาร์จนั้นๆ
			if evse.MaxAmperes > 0 && targetAmp > evse.MaxAmperes {
				targetAmp = evse.MaxAmperes
			}

			// 🎯 3. บันทึกผลลง Key ของ Connector นั้นๆ (เช่น "CS-SIEMENS_2" จะแยกกันชัดเจน ไม่โดนเขียนทับ)
			allocations[key] = targetAmp
			allocations[evse.SerialNumber] = targetAmp // เผื่อต้องการใช้ Key สั้นด้วย

			totalAllocated += targetAmp
		} else {
			// หัวชาร์จที่ไม่ได้ชาร์จอยู่ ให้โควตา 0A
			allocations[key] = 0.0
			allocations[evse.SerialNumber] = 0.0
		}
	}

	return DLBResult{
		Allocations:    allocations,
		TotalAllocated: totalAllocated,
	}
}