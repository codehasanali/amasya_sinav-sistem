package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"github.com/rs/cors"
	"api/db"
	"strconv"
	"github.com/gorilla/websocket"
	"time"
	"fmt"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// WebSocket message types
const (
	ExamCreate  = "EXAM_CREATE"
	ExamUpdate  = "EXAM_UPDATE"
	ExamDelete  = "EXAM_DELETE"
	ExamList    = "EXAM_LIST"
	ExamDetails = "EXAM_DETAILS" // Added new message type for exam details
)

// WebSocket message structure
type WSMessage struct {
	Type        string      `json:"type"`
	Action      string      `json:"action"`
	Data        interface{} `json:"data"`
	DepartmentID int        `json:"departmentId"`
}

// Client represents a WebSocket client
type Client struct {
	hub         *Hub
	conn        *websocket.Conn
	send        chan []byte
	departmentID int
}

// Hub manages WebSocket connections
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
}

func newHub() *Hub {
	return &Hub{
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("New client registered for department %d", client.departmentID)
			
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				log.Printf("Client unregistered from department %d", client.departmentID)
			}
			
		case message := <-h.broadcast:
			var wsMessage WSMessage
			if err := json.Unmarshal(message, &wsMessage); err != nil {
				log.Printf("Error unmarshaling broadcast message: %v", err)
				continue
			}
			
			for client := range h.clients {
				// Only send to clients in the same department
				if client.departmentID == wsMessage.DepartmentID {
					select {
					case client.send <- message:
					default:
						close(client.send)
						delete(h.clients, client)
					}
				}
			}
		}
	}
}

func handleWebSocket(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		departmentID := r.URL.Query().Get("departmentId")
		if departmentID == "" {
			log.Println("WebSocket connection attempt without departmentId")
			http.Error(w, "Department ID is required", http.StatusBadRequest)
			return
		}
		
		deptID, err := strconv.Atoi(departmentID)
		if err != nil {
			log.Printf("Invalid department ID: %v", err)
			http.Error(w, "Invalid department ID", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Error upgrading connection: %v", err)
			return
		}

		client := &Client{
			hub:         hub,
			conn:        conn,
			send:        make(chan []byte, 256),
			departmentID: deptID,
		}
		
		client.hub.register <- client

		// Send initial connection message
		welcomeMsg := WSMessage{
			Type: "CONNECTED",
			Data: map[string]interface{}{
				"message": "Connected to exam updates",
				"departmentId": deptID,
			},
			DepartmentID: deptID,
		}
		
		if msgBytes, err := json.Marshal(welcomeMsg); err == nil {
			client.send <- msgBytes
		}

		go client.writePump()
		go client.readPump()

		log.Printf("New WebSocket connection established for department %d", deptID)
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			break
		}

		var wsMessage WSMessage
		if err := json.Unmarshal(message, &wsMessage); err != nil {
			log.Printf("Error unmarshaling message: %v", err)
			continue
		}

		wsMessage.DepartmentID = c.departmentID
		if updatedMessage, err := json.Marshal(wsMessage); err == nil {
			c.hub.broadcast <- updatedMessage
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Exam events için helper fonksiyon
func notifyExamChange(hub *Hub, eventType string, data interface{}, departmentID int) {
	if hub == nil {
		return
	}

	update := WSMessage{
		Type:        eventType,
		Data:        data,
		DepartmentID: departmentID,
	}
	
	message, err := json.Marshal(update)
	if err != nil {
		log.Printf("Error marshaling exam update: %v", err)
		return
	}
	
	hub.broadcast <- message
}

// Helper function to broadcast updates to WebSocket clients
func broadcastUpdate(hub *Hub, eventType string, data interface{}, departmentID int) {
	if hub == nil {
		return
	}

	update := WSMessage{
		Type:         eventType,
		Data:         data,
		DepartmentID: departmentID,
	}
	
	message, err := json.Marshal(update)
	if err != nil {
		log.Printf("Error marshaling broadcast update: %v", err)
		return
	}
	
	hub.broadcast <- message
}

// Helper function to send error response in JSON format
func sendErrorResponse(w http.ResponseWriter, message string, statusCode int) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(statusCode)
    json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// HTTP handler'ı ile Socket notification'ı birleştiren fonksiyon
func handleExamsWithNotification(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // CORS headers'ı ekle
        w.Header().Set("Content-Type", "application/json")
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

        if r.Method == "OPTIONS" {
            w.WriteHeader(http.StatusOK)
            return
        }

        ctx := context.Background()
        departmentId := r.URL.Query().Get("departmentId")
        if departmentId == "" {
            sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
            return
        }

        deptID, err := strconv.Atoi(departmentId)
        if err != nil {
            sendErrorResponse(w, "Invalid department ID", http.StatusBadRequest)
            return
        }

        exams, err := client.Exam.FindMany(
            db.Exam.DepartmentID.Equals(deptID),
        ).With(
            db.Exam.Course.Fetch(),
            db.Exam.TeacherInCharge.Fetch(),
            db.Exam.ExamRooms.Fetch(),
            db.Exam.Supervisors.Fetch(),
        ).Exec(ctx)
        
        if err != nil {
            sendErrorResponse(w, fmt.Sprintf("Error fetching exams: %v", err), http.StatusInternalServerError)
            return
        }

        response := map[string]interface{}{
            "success": true,
            "data": exams,
        }

        if err := json.NewEncoder(w).Encode(response); err != nil {
            sendErrorResponse(w, fmt.Sprintf("Error encoding response: %v", err), http.StatusInternalServerError)
            return
        }
    }
}

func main() {
	client := db.NewClient()
	if err := client.Prisma.Connect(); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := client.Prisma.Disconnect(); err != nil {
			panic(err)
		}
	}()

	// Örnek verileri oluştur
	ctx := context.Background()
	createSampleData(ctx, client)

	// CORS yapılandırması
	corsOptions := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
			AllowedMethods: []string{
				http.MethodGet,
				http.MethodPost,
				http.MethodPut,
				http.MethodPatch,
				http.MethodDelete,
				http.MethodOptions,
			},
			AllowedHeaders: []string{
				"Accept",
				"Authorization",
				"Content-Type",
				"X-CSRF-Token",
			},
			ExposedHeaders:   []string{"Link"},
			AllowCredentials: true,
			MaxAge:          300,
	})

	// API rotaları
	mux := http.NewServeMux()
	
	// HTML dosyaları için
	fs := http.FileServer(http.Dir("html"))
	mux.Handle("/html/", http.StripPrefix("/html/", fs))
	mux.Handle("/", http.RedirectHandler("/html/login.html", http.StatusSeeOther))
	
	// API rotaları
	mux.HandleFunc("/api/teachers", handleTeachers(client))
	mux.HandleFunc("/api/courses", handleCourses(client))
	mux.HandleFunc("/api/examrooms", handleExamRooms(client, newHub()))
	mux.HandleFunc("/api/department/login", handleDepartmentLogin(client))
	mux.HandleFunc("/api/exams/create", handleCreateExam(client))
	mux.HandleFunc("/api/exams/list", handleExamsWithNotification(client, newHub()))
	mux.HandleFunc("/api/exams/details", handleExamDetails(client)) // Added new endpoint for exam details

	hub := newHub()
	go hub.run()

	// Add WebSocket endpoint
	mux.HandleFunc("/ws", handleWebSocket(hub))

	handler := corsOptions.Handler(mux)

	log.Println("Server 8080 portunda başlatılıyor...")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

// New handler for exam details
func handleExamDetails(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        examID := r.URL.Query().Get("examId")
        if examID == "" {
            sendErrorResponse(w, "Exam ID is required", http.StatusBadRequest)
            return
        }

        id, err := strconv.Atoi(examID)
        if err != nil {
            sendErrorResponse(w, "Invalid exam ID", http.StatusBadRequest)
            return
        }

        ctx := context.Background()
        exam, err := client.Exam.FindUnique(
            db.Exam.ID.Equals(id),
        ).With(
            db.Exam.Course.Fetch(),
            db.Exam.TeacherInCharge.Fetch(),
            db.Exam.ExamRooms.Fetch(),
            db.Exam.Supervisors.Fetch(),
        ).Exec(ctx)

        if err != nil {
            sendErrorResponse(w, fmt.Sprintf("Error fetching exam details: %v", err), http.StatusInternalServerError)
            return
        }

        if exam == nil {
            sendErrorResponse(w, "Exam not found", http.StatusNotFound)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "data": exam,
        })
    }
}

func handleDepartmentLogin(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        var loginData struct {
            Username string `json:"username"`
            Password string `json:"password"`
        }

        if err := json.NewDecoder(r.Body).Decode(&loginData); err != nil {
            sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
            return
        }

        ctx := context.Background()
        department, err := client.Department.FindFirst(
            db.Department.AdminUsername.Equals(loginData.Username),
            db.Department.AdminPassword.Equals(loginData.Password),
        ).Exec(ctx)

        if err != nil || department == nil {
            sendErrorResponse(w, "Invalid credentials", http.StatusUnauthorized)
            return
        }

        // Generate a simple token (in production, use a proper JWT implementation)
        token := fmt.Sprintf("%d_%d", department.ID, time.Now().Unix())

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "department": map[string]interface{}{
                "id": department.ID,
                "name": department.Name,
            },
            "token": token,
        })
    }
}

func handleTeachers(client *db.PrismaClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		departmentId := r.URL.Query().Get("departmentId")
		if departmentId == "" {
			sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
			return
		}

		deptID, _ := strconv.Atoi(departmentId)
		teachers, err := client.Teacher.FindMany(
			db.Teacher.DepartmentID.Equals(deptID),
		).Exec(ctx)
		
		if err != nil {
			sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(teachers)
	}
}

func handleCourses(client *db.PrismaClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		departmentId := r.URL.Query().Get("departmentId")
		if departmentId == "" {
			sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
			return
		}

		deptID, _ := strconv.Atoi(departmentId)
		courses, err := client.Course.FindMany(
			db.Course.DepartmentID.Equals(deptID),
		).Exec(ctx)
		
		if err != nil {
			sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(courses)
	}
}

func handleExamRooms(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		switch r.Method {
		case http.MethodGet:
			departmentId := r.URL.Query().Get("departmentId")
			if departmentId == "" {
				sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
				return
			}

			deptID, _ := strconv.Atoi(departmentId)
			
			rooms, err := client.ExamRoom.FindMany(
				db.ExamRoom.DepartmentID.Equals(deptID),
			).Exec(ctx)
			
			if err != nil {
				sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(rooms)

		case http.MethodPost:
			var roomData struct {
				RoomCode    string `json:"roomCode"`
				Capacity    int    `json:"capacity"`
				DepartmentID int   `json:"departmentId"`
			}

			if err := json.NewDecoder(r.Body).Decode(&roomData); err != nil {
				sendErrorResponse(w, err.Error(), http.StatusBadRequest)
				return
			}

			room, err := client.ExamRoom.CreateOne(
				db.ExamRoom.RoomCode.Set(roomData.RoomCode),
				db.ExamRoom.Department.Link(db.Department.ID.Equals(roomData.DepartmentID)),
			).Exec(ctx)

			if err != nil {
				sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
				return
			}

			broadcastUpdate(hub, "examRoom", room, roomData.DepartmentID)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(room)

		case http.MethodPut:
			var roomData struct {
				ID          int    `json:"id"`
				RoomCode    string `json:"roomCode"`
				DepartmentID int   `json:"departmentId"`
			}

			if err := json.NewDecoder(r.Body).Decode(&roomData); err != nil {
				sendErrorResponse(w, err.Error(), http.StatusBadRequest)
				return
			}

			room, err := client.ExamRoom.FindUnique(
				db.ExamRoom.ID.Equals(roomData.ID),
			).Update(
				db.ExamRoom.RoomCode.Set(roomData.RoomCode),
			).Exec(ctx)

			if err != nil {
				sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
				return
			}

			broadcastUpdate(hub, "examRoom", room, roomData.DepartmentID)
			json.NewEncoder(w).Encode(room)

		case http.MethodDelete:
			roomID := r.URL.Query().Get("id")
			if roomID == "" {
				sendErrorResponse(w, "Room ID is required", http.StatusBadRequest)
				return
			}

			id, _ := strconv.Atoi(roomID)
			_, err := client.ExamRoom.FindUnique(
				db.ExamRoom.ID.Equals(id),
			).Delete().Exec(ctx)

			if err != nil {
				sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
				return
			}

			broadcastUpdate(hub, "examRoom", roomID, 0)
			w.WriteHeader(http.StatusOK)
		}
	}
}

// Exam yapısı
type ExamRequest struct {
    CourseID          int       `json:"courseId"`
    TeacherID         int       `json:"teacherId"`
    ExamRoomID        int       `json:"examRoomId"`
    StartTime         time.Time `json:"startTime"`
    EndTime           time.Time `json:"endTime"`
    DepartmentID      int       `json:"departmentId"`
    SupervisorIDs     []int     `json:"supervisorIds"`
}

func handleCreateExam(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        ctx := context.Background()
        var examData ExamRequest
        if err := json.NewDecoder(r.Body).Decode(&examData); err != nil {
            log.Printf("Error decoding exam data: %v", err)
            sendErrorResponse(w, fmt.Sprintf("Invalid exam data: %v", err), http.StatusBadRequest)
            return
        }

        // Log the received data
        log.Printf("Received exam data: %+v", examData)

        // Validate required fields
        if examData.CourseID == 0 || examData.TeacherID == 0 || examData.ExamRoomID == 0 || examData.DepartmentID == 0 {
            log.Printf("Missing required fields: CourseID=%d, TeacherID=%d, ExamRoomID=%d, DepartmentID=%d",
                examData.CourseID, examData.TeacherID, examData.ExamRoomID, examData.DepartmentID)
            sendErrorResponse(w, "Missing required fields", http.StatusBadRequest)
            return
        }

        // Create exam with relations
        exam, err := client.Exam.CreateOne(
            db.Exam.Name.Set(fmt.Sprintf("%s Sınavı", examData.StartTime.Format("02.01.2006"))),
            db.Exam.StartTime.Set(examData.StartTime),
            db.Exam.EndTime.Set(examData.EndTime),
            db.Exam.Date.Set(examData.StartTime),
            db.Exam.Course.Link(
                db.Course.ID.Equals(examData.CourseID),
            ),
            db.Exam.Department.Link(
                db.Department.ID.Equals(examData.DepartmentID),
            ),
            db.Exam.TeacherInCharge.Link(
                db.Teacher.ID.Equals(examData.TeacherID),
            ),
            db.Exam.MaxStudentCount.Set(0),
            db.Exam.Status.Set("PENDING"),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error creating exam: %v", err)
            sendErrorResponse(w, fmt.Sprintf("Error creating exam: %v", err), http.StatusInternalServerError)
            return
        }

        // Link exam room in a separate step
        exam, err = client.Exam.FindUnique(
            db.Exam.ID.Equals(exam.ID),
        ).Update(
            db.Exam.ExamRooms.Link(
                db.ExamRoom.ID.Equals(examData.ExamRoomID),
            ),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error linking exam room: %v", err)
            sendErrorResponse(w, fmt.Sprintf("Error linking exam room: %v", err), http.StatusInternalServerError)
            return
        }

        // Add supervisors one by one in separate steps
        if len(examData.SupervisorIDs) > 0 {
            for _, supervisorID := range examData.SupervisorIDs {
                exam, err = client.Exam.FindUnique(
                    db.Exam.ID.Equals(exam.ID),
                ).Update(
                    db.Exam.Supervisors.Link(
                        db.Teacher.ID.Equals(supervisorID),
                    ),
                ).Exec(ctx)

                if err != nil {
                    log.Printf("Error adding supervisor %d: %v", supervisorID, err)
                    sendErrorResponse(w, fmt.Sprintf("Error adding supervisor %d: %v", supervisorID, err), http.StatusInternalServerError)
                    return
                }
            }
        }

        // Fetch the complete exam data with all relations
        exam, err = client.Exam.FindUnique(
            db.Exam.ID.Equals(exam.ID),
        ).With(
            db.Exam.Course.Fetch(),
            db.Exam.TeacherInCharge.Fetch(),
            db.Exam.ExamRooms.Fetch(),
            db.Exam.Supervisors.Fetch(),
            db.Exam.Department.Fetch(),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error fetching complete exam data: %v", err)
            sendErrorResponse(w, fmt.Sprintf("Error fetching complete exam data: %v", err), http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusCreated)
        json.NewEncoder(w).Encode(exam)
    }
}

func createSampleData(ctx context.Context, client *db.PrismaClient) {
	// Bölüm oluşturma
	department, err := client.Department.CreateOne(
		db.Department.Name.Set("Bilgisayar Mühendisliği"),
		db.Department.AdminUsername.Set("pcmuh"),
		db.Department.AdminPassword.Set("pcmuh123"),
	).Exec(ctx)

	if err != nil {
		log.Printf("Bölüm oluşturma hatası: %v", err)
		return
	}

	// Sınav yerleri oluşturma
	examRooms := []struct {
		code string
	}{
		{"A101"},
		{"A102"},
		{"A130"},
	}

	for _, room := range examRooms {
		_, err = client.ExamRoom.CreateOne(
			db.ExamRoom.RoomCode.Set(room.code),
			db.ExamRoom.Department.Link(db.Department.ID.Equals(department.ID)),
		).Exec(ctx)
		if err != nil {
			log.Printf("Sınav yeri oluşturma hatası: %v", err)
		}
	}

	// Öğretmenler oluşturma
	teachers := []struct {
		name  string
		title string
	}{
		{"Hasan Ali Arıkan", "Prof."},
		{"Mehmet Yılmaz", "Doç."},
		{"Ayşe Demir", "Dr."},
	}

	for _, t := range teachers {
		_, err = client.Teacher.CreateOne(
			db.Teacher.Name.Set(t.name),
			db.Teacher.Title.Set(t.title),
			db.Teacher.Department.Link(db.Department.ID.Equals(department.ID)),
		).Exec(ctx)
		if err != nil {
			log.Printf("Öğretmen oluşturma hatası: %v", err)
		}
	}

	// Dersler oluşturma
	courses := []string{
		"Bilgisayar Mühendisliğine Giriş",
		"Veri Yapıları",
		"Algoritma Analizi",
		"Veritabanı Yönetim Sistemleri",
	}

	for _, courseName := range courses {
		_, err = client.Course.CreateOne(
			db.Course.Name.Set(courseName),
			db.Course.Department.Link(db.Department.ID.Equals(department.ID)),
		).Exec(ctx)
		if err != nil {
			log.Printf("Ders oluşturma hatası: %v", err)
		}
	}

	log.Println("Örnek veriler başarıyla oluşturuldu!")
}

func handleExamUpdates(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx := context.Background()
        departmentId := r.URL.Query().Get("departmentId")
        if departmentId == "" {
            sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
            return
        }

        deptID, _ := strconv.Atoi(departmentId)
        exams, err := client.Exam.FindMany(
            db.Exam.DepartmentID.Equals(deptID),
        ).With(
            db.Exam.Course.Fetch(),
            db.Exam.TeacherInCharge.Fetch(),
            db.Exam.ExamRooms.Fetch(),
            db.Exam.Supervisors.Fetch(),
        ).Exec(ctx)
        
        if err != nil {
            sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
            return
        }

        // Detaylı sınav bilgilerini içeren yapı
        type ExamResponse struct {
            ID          int       `json:"id"`
            Name        string    `json:"name"`
            StartTime   time.Time `json:"startTime"`
            EndTime     time.Time `json:"endTime"`
            Course      struct {
                ID   int    `json:"id"`
                Name string `json:"name"`
            } `json:"course"`
            Teacher     struct {
                ID    int    `json:"id"`
                Name  string `json:"name"`
                Title string `json:"title"`
            } `json:"teacher"`
            Room        struct {
                ID       int    `json:"id"`
                RoomCode string `json:"roomCode"`
            } `json:"room"`
            Supervisors []struct {
                ID    int    `json:"id"`
                Name  string `json:"name"`
                Title string `json:"title"`
            } `json:"supervisors"`
        }

        // Yanıt verilerini oluştur
        var response []ExamResponse
        for _, exam := range exams {
            examResp := ExamResponse{
                ID:        exam.ID,
                Name:      exam.Name,
                StartTime: exam.StartTime,
                EndTime:   exam.EndTime,
            }

            course := exam.Course()
            if course != nil {
                examResp.Course.ID = course.ID
                examResp.Course.Name = course.Name
            }

            teacher := exam.TeacherInCharge()
            if teacher != nil {
                examResp.Teacher.ID = teacher.ID
                examResp.Teacher.Name = teacher.Name
                examResp.Teacher.Title = teacher.Title
            }

            examRooms := exam.ExamRooms()
            if len(examRooms) > 0 {
                examResp.Room.ID = examRooms[0].ID
                examResp.Room.RoomCode = examRooms[0].RoomCode
            }

            supervisors := exam.Supervisors()
            if len(supervisors) > 0 {
                for _, supervisor := range supervisors {
                    examResp.Supervisors = append(examResp.Supervisors, struct {
                        ID    int    `json:"id"`
                        Name  string `json:"name"`
                        Title string `json:"title"`
                    }{
                        ID:    supervisor.ID,
                        Name:  supervisor.Name,
                        Title: supervisor.Title,
                    })
                }
            }

            response = append(response, examResp)
        }

        // Notify all clients in the department about the exam list
        notifyExamChange(hub, ExamList, response, deptID)
        json.NewEncoder(w).Encode(response)
    }
}
