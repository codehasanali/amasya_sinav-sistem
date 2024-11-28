package main

import (
	"api/db"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/cors"
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
	ExamCreate    = "EXAM_CREATE"
	ExamUpdate    = "EXAM_UPDATE"
	ExamDelete    = "EXAM_DELETE"
	ExamList      = "EXAM_LIST"
	ExamDetails   = "EXAM_DETAILS"
	CourseCreate  = "COURSE_CREATE"
	CourseUpdate  = "COURSE_UPDATE"
	CourseDelete  = "COURSE_DELETE"
	TeacherCreate = "TEACHER_CREATE"
	TeacherUpdate = "TEACHER_UPDATE"
	TeacherDelete = "TEACHER_DELETE"
)

// WebSocket message structure
type WSMessage struct {
	Type         string      `json:"type"`
	Action       string      `json:"action"`
	Data         interface{} `json:"data"`
	DepartmentID int         `json:"departmentId"`
}

type Client struct {
	hub          *Hub
	conn         *websocket.Conn
	send         chan []byte
	departmentID int
}

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
			hub:          hub,
			conn:         conn,
			send:         make(chan []byte, 256),
			departmentID: deptID,
		}

		client.hub.register <- client

		// Send initial connection message
		welcomeMsg := WSMessage{
			Type: "CONNECTED",
			Data: map[string]interface{}{
				"message":      "Connected to exam updates",
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
		Type:         eventType,
		Data:         data,
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
			"data":    exams,
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error encoding response: %v", err), http.StatusInternalServerError)
			return
		}
	}
}


func handleExamRooms(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx := context.Background()

        switch r.Method {
        case http.MethodGet:
            departmentID := r.URL.Query().Get("departmentId")
            if departmentID == "" {
                sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
                return
            }

            deptID, err := strconv.Atoi(departmentID)
            if err != nil {
                sendErrorResponse(w, "Invalid department ID", http.StatusBadRequest)
                return
            }

            rooms, err := client.ExamRoom.FindMany(
                db.ExamRoom.Department.Where(
                    db.Department.ID.Equals(deptID),
                ),
            ).Exec(ctx)

            if err != nil {
                sendErrorResponse(w, "Error fetching exam rooms", http.StatusInternalServerError)
                return
            }

            w.Header().Set("Content-Type", "application/json")
            json.NewEncoder(w).Encode(map[string]interface{}{
                "success": true,
                "data":    rooms,
            })

        case http.MethodPost:
            var roomData struct {
                RoomCode     string `json:"roomCode"`
                Capacity     int    `json:"capacity"`
                DepartmentID int    `json:"departmentId"`
            }

            if err := json.NewDecoder(r.Body).Decode(&roomData); err != nil {
                sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
                return
            }

            // First create with required fields
            room, err := client.ExamRoom.CreateOne(
                db.ExamRoom.RoomCode.Set(roomData.RoomCode),
                db.ExamRoom.Department.Link(
                    db.Department.ID.Equals(roomData.DepartmentID),
                ),
            ).Exec(ctx)

            if err != nil {
                log.Printf("Error creating exam room: %v", err)
                sendErrorResponse(w, "Error creating exam room", http.StatusInternalServerError)
                return
            }

            // Then update with capacity if needed
            if roomData.Capacity > 0 {
                room, err = client.ExamRoom.FindUnique(
                    db.ExamRoom.ID.Equals(room.ID),
                ).Update(
                    db.ExamRoom.Capacity.Set(roomData.Capacity),
                ).Exec(ctx)

                if err != nil {
                    log.Printf("Error updating room capacity: %v", err)
                }
            }

            // Notify clients about the new room
            notifyRoomChange(hub, "ROOM_CREATE", room, roomData.DepartmentID)

            w.Header().Set("Content-Type", "application/json")
            json.NewEncoder(w).Encode(map[string]interface{}{
                "success": true,
                "data":    room,
            })

        default:
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
        }
    }
}
func notifyRoomChange(hub *Hub, action string, data interface{}, departmentID int) {
    message := WSMessage{
        Type:         "ROOM_CHANGE",
        Action:       action,
        Data:         data,
        DepartmentID: departmentID,
    }

    if messageBytes, err := json.Marshal(message); err == nil {
        hub.broadcast <- messageBytes
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

	// Create a single hub instance
	hub := newHub()
	go hub.run()

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
		MaxAge:           300,
	})

	// API rotaları
	mux := http.NewServeMux()

	// HTML dosyaları için
	fs := http.FileServer(http.Dir("html"))
	mux.Handle("/html/", http.StripPrefix("/html/", fs))
	mux.Handle("/", http.RedirectHandler("/html/login.html", http.StatusSeeOther))
	mux.HandleFunc("/api/courses", handleCourses(client, hub))
	mux.HandleFunc("/api/examrooms", handleExamRooms(client, hub))
	mux.HandleFunc("/api/department/login", handleDepartmentLogin(client))
	mux.HandleFunc("/api/exams/list", handleExamsWithNotification(client, hub))
	mux.HandleFunc("/api/exams/create", handleCreateExam(client, hub))
	mux.HandleFunc("/api/exams/details", handleExamDetails(client))
	mux.HandleFunc("/api/teachers/department", handleTeachersByDepartment(client))
	mux.HandleFunc("/api/teachers/delete", handleDeleteTeacher(client, hub))
	mux.HandleFunc("/api/teachers/create", handleTeacherCreate(client, hub))
	mux.HandleFunc("/api/teachers/update", handleTeacherUpdate(client, hub))
	mux.HandleFunc("/api/courses/department", handleCoursesByDepartment(client))
	mux.HandleFunc("/api/courses/create", handleCourseCreate(client, hub))
	mux.HandleFunc("/api/courses/update", handleCourseUpdate(client, hub))
	mux.HandleFunc("/api/courses/delete", handleCourseDelete(client, hub))
	mux.HandleFunc("/api/superadmin/login", handleSuperAdminLogin(client))
	mux.HandleFunc("/api/superadmin/departments", handleDepartments(client))
	mux.HandleFunc("/api/superadmin/departments/create", handleCreateDepartment(client))
	mux.HandleFunc("/api/superadmin/departments/delete", handleDeleteDepartment(client))
	mux.HandleFunc("/api/departments", handleDepartments(client))

	mux.HandleFunc("/ws", handleWebSocket(hub))

	handler := corsOptions.Handler(mux)

	log.Println("Server 8080 portunda başlatılıyor...")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func handleDeleteTeacher(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Set headers
		w.Header().Set("Content-Type", "application/json")

		// Get teacher ID from URL path
		teacherID := r.URL.Query().Get("id")
		if teacherID == "" {
			sendErrorResponse(w, "Teacher ID is required", http.StatusBadRequest)
			return
		}

		id, err := strconv.Atoi(teacherID)
		if err != nil {
			sendErrorResponse(w, "Invalid teacher ID", http.StatusBadRequest)
			return
		}

		ctx := context.Background()

		// Get department ID before deleting the teacher
		teacher, err := client.Teacher.FindUnique(
			db.Teacher.ID.Equals(id),
		).Exec(ctx)

		if err != nil {
			log.Printf("Error finding teacher: %v", err)
			sendErrorResponse(w, "Error finding teacher", http.StatusInternalServerError)
			return
		}

		if teacher == nil {
			sendErrorResponse(w, "Teacher not found", http.StatusNotFound)
			return
		}

		departmentID := teacher.DepartmentID

		// Delete the teacher
		_, err = client.Teacher.FindUnique(
			db.Teacher.ID.Equals(id),
		).Delete().Exec(ctx)

		if err != nil {
			log.Printf("Error deleting teacher: %v", err)
			sendErrorResponse(w, "Error deleting teacher", http.StatusInternalServerError)
			return
		}

		// Broadcast the delete event
		broadcastUpdate(hub, TeacherDelete, map[string]interface{}{
			"id": id,
		}, departmentID)

		// Send successful response
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Teacher deleted successfully",
		})
	}
}

func handleCreateExam(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
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

		// Broadcast the create event
		broadcastUpdate(hub, ExamCreate, exam, examData.DepartmentID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(exam)
	}
}

func handleTeachersByDepartment(client *db.PrismaClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Set headers
		w.Header().Set("Content-Type", "application/json")

		// Get department ID from query params
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

		ctx := context.Background()

		// Fetch teachers for the department
		teachers, err := client.Teacher.FindMany(
			db.Teacher.DepartmentID.Equals(deptID),
		).With(
			db.Teacher.Department.Fetch(),
		).Exec(ctx)

		if err != nil {
			log.Printf("Error fetching teachers: %v", err)
			sendErrorResponse(w, "Error fetching teachers", http.StatusInternalServerError)
			return
		}

		// Format response
		type TeacherResponse struct {
			ID             int    `json:"id"`
			Name           string `json:"name"`
			Title          string `json:"title"`
			DepartmentID   int    `json:"departmentId"`
			DepartmentName string `json:"departmentName,omitempty"`
		}

		var response []TeacherResponse
		for _, teacher := range teachers {
			dept := teacher.Department()
			teacherResp := TeacherResponse{
				ID:           teacher.ID,
				Name:         teacher.Name,
				Title:        teacher.Title,
				DepartmentID: teacher.DepartmentID,
			}
			if dept != nil {
				teacherResp.DepartmentName = dept.Name
			}
			response = append(response, teacherResp)
		}

		// Send successful response
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    response,
		})
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
			"data":    exam,
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
				"id":   department.ID,
				"name": department.Name,
			},
			"token": token,
		})
	}
}

func handleCourses(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
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



type ExamRequest struct {
	CourseID      int       `json:"courseId"`
	TeacherID     int       `json:"teacherId"`
	ExamRoomID    int       `json:"examRoomId"`
	StartTime     time.Time `json:"startTime"`
	EndTime       time.Time `json:"endTime"`
	DepartmentID  int       `json:"departmentId"`
	SupervisorIDs []int     `json:"supervisorIds"`
}

func createSampleData(ctx context.Context, client *db.PrismaClient) {
	department, err := client.Department.CreateOne(
		db.Department.Name.Set("Bilgisayar Mühendisliği"),
		db.Department.AdminUsername.Set("pcmuh_"+strconv.FormatInt(time.Now().Unix(), 10)),
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

	// Öretmenler oluşturma
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

type TeacherRequest struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Title        string `json:"title"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	DepartmentId int    `json:"departmentId"`
}

func handleTeacherCreate(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Get authorization token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		parts := strings.Split(token, "_")
		if len(parts) != 2 {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get department ID from token
		departmentID, err := strconv.Atoi(parts[0])
		if err != nil {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.Background()

		// Verify department exists
		department, err := client.Department.FindUnique(
			db.Department.ID.Equals(departmentID),
		).Exec(ctx)

		if err != nil || department == nil {
			sendErrorResponse(w, "Department not found", http.StatusUnauthorized)
			return
		}

		// Parse request body
		var teacherData TeacherRequest
		if err := json.NewDecoder(r.Body).Decode(&teacherData); err != nil {
			sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if teacherData.Name == "" || teacherData.Title == "" {
			sendErrorResponse(w, "Name and title are required", http.StatusBadRequest)
			return
		}

		// Create teacher d
		teacher, err := client.Teacher.CreateOne(
			db.Teacher.Name.Set(teacherData.Name),
			db.Teacher.Title.Set(teacherData.Title),
			db.Teacher.Department.Link(
				db.Department.ID.Equals(departmentID),
			),
		).Exec(ctx)

		if err == nil && teacherData.Email != "" {
			teacher, err = client.Teacher.FindUnique(
				db.Teacher.ID.Equals(teacher.ID),
			).Update(
				db.Teacher.Email.Set(teacherData.Email),
				db.Teacher.Phone.Set(teacherData.Phone),
			).Exec(ctx)
		}

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error creating teacher: %v", err), http.StatusInternalServerError)
			return
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Teacher created successfully",
			"data":    teacher,
		})
	}
}

func handleTeacherUpdate(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Get teacher ID from query parameter
		teacherID := r.URL.Query().Get("id")
		if teacherID == "" {
			sendErrorResponse(w, "Teacher ID is required", http.StatusBadRequest)
			return
		}

		id, err := strconv.Atoi(teacherID)
		if err != nil {
			sendErrorResponse(w, "Invalid teacher ID", http.StatusBadRequest)
			return
		}

		// Get authorization token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		parts := strings.Split(token, "_")
		if len(parts) != 2 {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get department ID from token
		departmentID, err := strconv.Atoi(parts[0])
		if err != nil {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.Background()

		// First verify that the teacher exists and belongs to the department
		existingTeacher, err := client.Teacher.FindFirst(
			db.Teacher.ID.Equals(id),
			db.Teacher.DepartmentID.Equals(departmentID),
		).Exec(ctx)

		if err != nil || existingTeacher == nil {
			sendErrorResponse(w, "Teacher not found or unauthorized", http.StatusNotFound)
			return
		}

		// Parse request body
		var teacherData TeacherRequest
		if err := json.NewDecoder(r.Body).Decode(&teacherData); err != nil {
			sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if teacherData.Name == "" || teacherData.Title == "" {
			sendErrorResponse(w, "Name and title are required", http.StatusBadRequest)
			return
		}

		// Update teacher with all fields
		teacher, err := client.Teacher.FindUnique(
			db.Teacher.ID.Equals(id),
		).Update(
			db.Teacher.Name.Set(teacherData.Name),
			db.Teacher.Title.Set(teacherData.Title),
			db.Teacher.Email.Set(teacherData.Email),
			db.Teacher.Phone.Set(teacherData.Phone),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error updating teacher: %v", err), http.StatusInternalServerError)
			return
		}

		// Get department info in a separate query
		department, err := client.Department.FindUnique(
			db.Department.ID.Equals(departmentID),
		).Exec(ctx)

		if err != nil {
			log.Printf("Error fetching department: %v", err)
		}

		// Prepare response data
		responseData := map[string]interface{}{
			"id":             teacher.ID,
			"name":           teacher.Name,
			"title":          teacher.Title,
			"email":          teacher.Email,
			"phone":          teacher.Phone,
			"departmentId":   teacher.DepartmentID,
			"departmentName": department.Name,
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Teacher updated successfully",
			"data":    responseData,
		})
	}
}

type CourseRequest struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	DepartmentId int    `json:"departmentId"`
}

func handleCourseCreate(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		parts := strings.Split(token, "_")
		if len(parts) != 2 {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get department ID from token
		departmentID, err := strconv.Atoi(parts[0])
		if err != nil {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.Background()

		// Parse request body
		var courseData CourseRequest
		if err := json.NewDecoder(r.Body).Decode(&courseData); err != nil {
			sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if courseData.Name == "" {
			sendErrorResponse(w, "Course name is required", http.StatusBadRequest)
			return
		}

		// Create course
		course, err := client.Course.CreateOne(
			db.Course.Name.Set(courseData.Name),
			db.Course.Department.Link(
				db.Department.ID.Equals(departmentID),
			),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error creating course: %v", err), http.StatusInternalServerError)
			return
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Course created successfully",
			"data":    course,
		})
	}
}

func handleCoursesByDepartment(client *db.PrismaClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Get department ID from query params
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

		ctx := context.Background()

		// Fetch courses for the department
		courses, err := client.Course.FindMany(
			db.Course.DepartmentID.Equals(deptID),
		).With(
			db.Course.Department.Fetch(),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error fetching courses: %v", err), http.StatusInternalServerError)
			return
		}

		// Format response
		type CourseResponse struct {
			ID             int    `json:"id"`
			Name           string `json:"name"`
			DepartmentID   int    `json:"departmentId"`
			DepartmentName string `json:"departmentName,omitempty"`
		}

		var response []CourseResponse
		for _, course := range courses {
			dept := course.Department()
			courseResp := CourseResponse{
				ID:           course.ID,
				Name:         course.Name,
				DepartmentID: course.DepartmentID,
			}
			if dept != nil {
				courseResp.DepartmentName = dept.Name
			}
			response = append(response, courseResp)
		}

		// Send successful response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}
}

func handleCourseUpdate(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		courseID := r.URL.Query().Get("id")
		if courseID == "" {
			sendErrorResponse(w, "Course ID is required", http.StatusBadRequest)
			return
		}

		id, err := strconv.Atoi(courseID)
		if err != nil {
			sendErrorResponse(w, "Invalid course ID", http.StatusBadRequest)
			return
		}

		// Get authorization token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		parts := strings.Split(token, "_")
		if len(parts) != 2 {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get department ID from token
		departmentID, err := strconv.Atoi(parts[0])
		if err != nil {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.Background()

		// First verify that the course exists and belongs to the department
		existingCourse, err := client.Course.FindFirst(
			db.Course.ID.Equals(id),
			db.Course.DepartmentID.Equals(departmentID),
		).Exec(ctx)

		if err != nil || existingCourse == nil {
			sendErrorResponse(w, "Course not found or unauthorized", http.StatusNotFound)
			return
		}

		// Parse request body
		var courseData CourseRequest
		if err := json.NewDecoder(r.Body).Decode(&courseData); err != nil {
			sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if courseData.Name == "" {
			sendErrorResponse(w, "Course name is required", http.StatusBadRequest)
			return
		}

		// Update course
		course, err := client.Course.FindUnique(
			db.Course.ID.Equals(id),
		).Update(
			db.Course.Name.Set(courseData.Name),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error updating course: %v", err), http.StatusInternalServerError)
			return
		}

		department, err := client.Department.FindUnique(
			db.Department.ID.Equals(departmentID),
		).Exec(ctx)

		if err != nil {
			log.Printf("Error fetching department: %v", err)
		}

		responseData := map[string]interface{}{
			"id":             course.ID,
			"name":           course.Name,
			"departmentId":   course.DepartmentID,
			"departmentName": department.Name,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Course updated successfully",
			"data":    responseData,
		})
	}
}

func handleCourseDelete(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		courseID := r.URL.Query().Get("id")
		if courseID == "" {
			sendErrorResponse(w, "Course ID is required", http.StatusBadRequest)
			return
		}

		id, err := strconv.Atoi(courseID)
		if err != nil {
			sendErrorResponse(w, "Invalid course ID", http.StatusBadRequest)
			return
		}

		ctx := context.Background()

	
		_, err = client.Course.FindUnique(
			db.Course.ID.Equals(id),
		).Delete().Exec(ctx)

		if err != nil {
			log.Printf("Error deleting course: %v", err)
			sendErrorResponse(w, "Error deleting course", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Course deleted successfully",
		})
	}
}

func handleExamDelete(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		examID := r.URL.Query().Get("id")
		if examID == "" {
			sendErrorResponse(w, "Exam ID is required", http.StatusBadRequest)
			return
		}

		id, err := strconv.Atoi(examID)
		if err != nil {
			sendErrorResponse(w, "Invalid exam ID", http.StatusBadRequest)
			return
		}

		// Get authorization token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		parts := strings.Split(token, "_")
		if len(parts) != 2 {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get department ID from token
		departmentID, err := strconv.Atoi(parts[0])
		if err != nil {
			sendErrorResponse(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.Background()

		// First verify that the exam exists and belongs to the department
		existingExam, err := client.Exam.FindFirst(
			db.Exam.ID.Equals(id),
			db.Exam.DepartmentID.Equals(departmentID),
		).Exec(ctx)

		if err != nil || existingExam == nil {
			sendErrorResponse(w, "Exam not found or unauthorized", http.StatusNotFound)
			return
		}

		// Delete the exam
		_, err = client.Exam.FindUnique(
			db.Exam.ID.Equals(id),
		).Delete().Exec(ctx)

		if err != nil {
			log.Printf("Error deleting exam: %v", err)
			sendErrorResponse(w, "Error deleting exam", http.StatusInternalServerError)
			return
		}

		// Send WebSocket notification about exam deletion
		notifyExamChange(hub, "EXAM_DELETE", id, departmentID)

		// Send successful response
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Exam deleted successfully",
		})
	}
}

func handleSuperAdminLogin(client *db.PrismaClient) http.HandlerFunc {
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

        if loginData.Username != "amasya" || loginData.Password != "amasya2006" {
            sendErrorResponse(w, "Invalid credentials", http.StatusUnauthorized)
            return
        }

        token := fmt.Sprintf("0_superadmin_%d", time.Now().Unix())

        // Send successful response
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "token":   token,
            "role":    "superadmin",
            "id":      0, 
        })
    }
}

func handleDepartments(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        // Verify super admin token
        authHeader := r.Header.Get("Authorization")
        if !verifySuperAdminToken(authHeader) {
            sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
            return
        }

        ctx := context.Background()
        departments, err := client.Department.FindMany().Exec(ctx)
        if err != nil {
            log.Printf("Error fetching departments: %v", err)
            sendErrorResponse(w, "Error fetching departments", http.StatusInternalServerError)
            return
        }

        // Set CORS headers
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
        
        // Set content type
        w.Header().Set("Content-Type", "application/json")
        
        // Send response with departments data
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "data":    departments,
        })
    }
}

func handleCreateDepartment(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        // Verify super admin token
        authHeader := r.Header.Get("Authorization")
        if !verifySuperAdminToken(authHeader) {
            sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
            return
        }

        var departmentData struct {
            Name           string `json:"name"`
            AdminUsername string `json:"adminUsername"`
            AdminPassword string `json:"adminPassword"`
        }

        if err := json.NewDecoder(r.Body).Decode(&departmentData); err != nil {
            sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
            return
        }

        ctx := context.Background()
        department, err := client.Department.CreateOne(
            db.Department.Name.Set(departmentData.Name),
            db.Department.AdminUsername.Set(departmentData.AdminUsername),
            db.Department.AdminPassword.Set(departmentData.AdminPassword),
        ).Exec(ctx)

        if err != nil {
            sendErrorResponse(w, "Error creating department", http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "data":    department,
        })
    }
}

func handleDeleteDepartment(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodDelete {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        // Verify super admin token
        authHeader := r.Header.Get("Authorization")
        if !verifySuperAdminToken(authHeader) {
            sendErrorResponse(w, "Unauthorized", http.StatusUnauthorized)
            return
        }

        departmentID := r.URL.Query().Get("id")
        if departmentID == "" {
            sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
            return
        }

        id, err := strconv.Atoi(departmentID)
        if err != nil {
            sendErrorResponse(w, "Invalid department ID", http.StatusBadRequest)
            return
        }

        ctx := context.Background()
        _, err = client.Department.FindUnique(
            db.Department.ID.Equals(id),
        ).Delete().Exec(ctx)

        if err != nil {
            sendErrorResponse(w, "Error deleting department", http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "message": "Department deleted successfully",
        })
    }
}

func verifySuperAdminToken(authHeader string) bool {
    if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
        return false
    }

    token := strings.TrimPrefix(authHeader, "Bearer ")
    parts := strings.Split(token, "_")
    
    // Token format should be: "0_superadmin_timestamp"
    if len(parts) != 3 || parts[0] != "0" || parts[1] != "superadmin" {
        return false
    }

    return true
}


