package main

import (
	"log"
	"net/http"
	"time"

	"csms-engine/internal/dlb"
	"csms-engine/internal/ocpp"
	"csms-engine/internal/store"

	"github.com/gin-gonic/gin"
)

// DTO สำหรับรับข้อมูลกระแสไฟที่วัดได้จริงจาก ENBox (ESP32)
type UpdateNowAmpDTO struct {
	AddressEsp32 string  `json:"address_esp32" binding:"required"`
	NowAmp       float64 `json:"now_amp"`
	MaxAmp       float64 `json:"max_amp"`
}

// DTO สำหรับลงทะเบียน ENBox ใหม่ลง RAM
// 🎯 แก้ให้ตรงกับ payload ใหม่: idevse เป็น array ของ {name, max_amp} และเปลี่ยน max_amp -> max_amp_build
type RegisterBoxDTO struct {
	AddressEsp32 string           `json:"address_esp32" binding:"required"`
	IDEVSE       []dlb.EVSEConfig `json:"idevse" binding:"required"`
	MaxAmpBuild  float64          `json:"max_amp_build,omitempty"`
}

func main() {
	// 1. Initialize Memory Store (RAM State Management)
	memStore := store.NewMemoryStore()

	// 2. Initialize OCPP 1.6 Handler
	ocppHandler := v16.NewOCPPHandler(memStore)

	// 3. Initialize Gin Engine
	r := gin.Default()

	// -------------------------------------------------------------------------
	// 🔌 1. WebSocket Route สำหรับตู้ชาร์จ (OCPP 1.6 Connection)
	// -------------------------------------------------------------------------
	r.GET("/ocpp/:serialNumber", ocppHandler.HandleWS)

	// -------------------------------------------------------------------------
	// 🌐 2. REST API Routes (สำหรับ ENBox / Dashboard / System Health)
	// -------------------------------------------------------------------------
	api := r.Group("/api/v1")
	{
		// Health Check Endpoint
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"status":    "online",
				"system":    "CSMS Engine Core",
				"timestamp": time.Now().Format(time.RFC3339),
			})
		})

		// API 1: ลงทะเบียน ENBox (ESP32) พร้อมเชื่อมตู้ชาร์จเข้า RAM ( saveToRam )
		api.POST("/box/register", func(c *gin.Context) {
			var dto RegisterBoxDTO
			if err := c.ShouldBindJSON(&dto); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			maxAmpBuild := dto.MaxAmpBuild
			if maxAmpBuild <= 0 {
				maxAmpBuild = 32.0 // Default 32A หากไม่ได้ระบุมา
			}

			box := dlb.ENBox{
				AddressEsp32: dto.AddressEsp32,
				IDEVSE:       dto.IDEVSE,
				MaxAmpBuild:  maxAmpBuild, // 🎯 เก็บลง box ตรงๆ ตามโครงสร้างใหม่
				NowAmp:       0,
			}

			// 🎯 SaveBoxToRam ยังรับ maxAmp แยกอยู่เหมือนเดิม (ไม่แก้ signature/logic ของ store)
			// ใช้ box.MaxAmpBuild เป็นค่าที่ส่งเข้าไปแทน field max_amp แบบเดิม
			savedBox := memStore.SaveBoxToRam(box, maxAmpBuild)
			c.JSON(http.StatusOK, gin.H{
				"message": "บันทึกข้อมูล ControllerBox ลง RAM สำเร็จ",
				"data":    savedBox,
			})
		})

		// API 2: รับค่ากระแสไฟวัดได้จาก ENBox (ESP32) -> คำนวณ DLB -> สั่งจ่ายไฟลงตู้ชาร์จทันที ( updateNowAmpAndSendWs )
		api.POST("/box/now-amp", func(c *gin.Context) {
			var dto UpdateNowAmpDTO
			if err := c.ShouldBindJSON(&dto); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			// กำหนดพิกัดกระแสไฟรวมสูงสุดของอาคาร (ตัวอย่าง: 100A)
			buildingCap := dlb.BuildingCapacity{
				MaxAmperes: 100.0,
			}

			// ประมวลผล DLB และส่งคำสั่ง OCPP SetChargingProfile ผ่าน WebSocket
			result, err := memStore.UpdateNowAmpAndSendWs(dto.AddressEsp32, dto.NowAmp, buildingCap)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, result)
		})

		// API 3: ดูข้อมูลทั้งหมดใน RAM รวมสถานะออนไลน์และ Connector ( getAllFromRam )
		api.GET("/box/ram-state", func(c *gin.Context) {
			data := memStore.GetAllFromRam()
			c.JSON(http.StatusOK, gin.H{
				"total_boxes": len(data),
				"data":        data,
			})
		})
	}

	// -------------------------------------------------------------------------
	// 🚀 3. สั่งเริ่มรัน HTTP & WebSocket Server ที่ Port 3000
	// -------------------------------------------------------------------------
	log.Println("==================================================")
	log.Println("🚀 CSMS Engine started successfully on :3000")
	log.Println("🔌 OCPP WebSocket URL: ws://localhost:3000/ocpp/:serialNumber")
	log.Println("🌐 REST API Base URL:  http://localhost:3000/api/v1")
	log.Println("==================================================")

	if err := r.Run(":3000"); err != nil {
		log.Fatalf("❌ Failed to start server: %v", err)
	}
}