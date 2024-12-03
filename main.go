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
	mux.HandleFunc("/api/exams/create", handleExamCreate(client, hub))
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
	mux.HandleFunc("/api/teachers/add-department", handleAddTeacherDepartment(client, hub))
	mux.HandleFunc("/api/teachers/remove", handleRemoveTeacherDepartment(client, hub))
	mux.HandleFunc("/api/exams/check-room-availability", handleCheckRoomAvailability(client))

	mux.HandleFunc("/api/exams/check-teacher-availability", handleCheckTeacherAvailability(client))

	mux.HandleFunc("/api/teachers/all", handleGetAllTeachers(client))

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

		// Get teacher with departments before deleting
		teacher, err := client.Teacher.FindUnique(
			db.Teacher.ID.Equals(id),
		).With(
			db.Teacher.Departments.Fetch(),
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

		// Store departments for broadcasting
		departments := teacher.Departments()

		// Delete the teacher
		_, err = client.Teacher.FindUnique(
			db.Teacher.ID.Equals(id),
		).Delete().Exec(ctx)

		if err != nil {
			log.Printf("Error deleting teacher: %v", err)
			sendErrorResponse(w, "Error deleting teacher", http.StatusInternalServerError)
			return
		}

		// Broadcast delete event to all related departments
		for _, dept := range departments {
			broadcastUpdate(hub, TeacherDelete, map[string]interface{}{
				"id": id,
			}, dept.ID)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Teacher deleted successfully",
		})
	}
}

func handleExamCreate(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var examData struct {
			Name          string    `json:"name"`
			CourseID      int       `json:"courseId"`
			TeacherID     int       `json:"teacherId"`
			ExamRoomIds   []int     `json:"examRoomIds"`
			StartTime     time.Time `json:"startTime"`
			EndTime       time.Time `json:"endTime"`
			Date          time.Time `json:"date"`
			DepartmentID  int       `json:"departmentId"`
			SupervisorIds []int     `json:"supervisorIds"`
		}

		if err := json.NewDecoder(r.Body).Decode(&examData); err != nil {
			sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validation
		if examData.Name == "" {
			sendErrorResponse(w, "Exam name is required", http.StatusBadRequest)
			return
		}
		if examData.CourseID == 0 {
			sendErrorResponse(w, "Course ID is required", http.StatusBadRequest)
			return
		}
		if examData.TeacherID == 0 {
			sendErrorResponse(w, "Teacher ID is required", http.StatusBadRequest)
			return
		}
		if len(examData.ExamRoomIds) == 0 {
			sendErrorResponse(w, "At least one exam room is required", http.StatusBadRequest)
			return
		}
		if examData.StartTime.IsZero() {
			sendErrorResponse(w, "Start time is required", http.StatusBadRequest)
			return
		}
		if examData.EndTime.IsZero() {
			sendErrorResponse(w, "End time is required", http.StatusBadRequest)
			return
		}
		if examData.Date.IsZero() {
			sendErrorResponse(w, "Date is required", http.StatusBadRequest)
			return
		}
		if examData.DepartmentID == 0 {
			sendErrorResponse(w, "Department ID is required", http.StatusBadRequest)
			return
		}

		ctx := context.Background()

		// Create exam with initial data
		exam, err := client.Exam.CreateOne(
			db.Exam.Name.Set(examData.Name),
			db.Exam.StartTime.Set(examData.StartTime),
			db.Exam.EndTime.Set(examData.EndTime),
			db.Exam.Date.Set(examData.Date),
			db.Exam.Course.Link(db.Course.ID.Equals(examData.CourseID)),
			db.Exam.Department.Link(db.Department.ID.Equals(examData.DepartmentID)),
			db.Exam.TeacherInCharge.Link(db.Teacher.ID.Equals(examData.TeacherID)),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error creating exam: %v", err), http.StatusInternalServerError)
			return
		}

		// Link exam rooms
		for _, roomID := range examData.ExamRoomIds {
			_, err = client.Exam.FindUnique(
				db.Exam.ID.Equals(exam.ID),
			).Update(
				db.Exam.ExamRooms.Link(
					db.ExamRoom.ID.Equals(roomID),
				),
			).Exec(ctx)

			if err != nil {
				log.Printf("Error linking room %d: %v", roomID, err)
				continue
			}
		}

		// Link supervisors if any
		if len(examData.SupervisorIds) > 0 {
			for _, supervisorID := range examData.SupervisorIds {
				_, err = client.Exam.FindUnique(
					db.Exam.ID.Equals(exam.ID),
				).Update(
					db.Exam.Supervisors.Link(
						db.Teacher.ID.Equals(supervisorID),
					),
				).Exec(ctx)

				if err != nil {
					log.Printf("Error linking supervisor %d: %v", supervisorID, err)
					continue
				}
			}
		}

		notifyExamChange(hub, ExamCreate, exam, examData.DepartmentID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    exam,
		})
	}
}

func handleTeachersByDepartment(client *db.PrismaClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

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

		teachers, err := client.Teacher.FindMany(
			db.Teacher.Departments.Some(
				db.Department.ID.Equals(deptID),
			),
		).With(
			db.Teacher.Departments.Fetch(),
		).Exec(ctx)

		if err != nil {
			log.Printf("Error fetching teachers: %v", err)
			sendErrorResponse(w, "Error fetching teachers", http.StatusInternalServerError)
			return
		}

		type TeacherResponse struct {
			ID            int    `json:"id"`
			Name          string `json:"name"`
			Title         string `json:"title"`
			Email         string `json:"email,omitempty"`
			Phone         string `json:"phone,omitempty"`
			Departments   []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"departments"`
		}

		var response []TeacherResponse
		for _, teacher := range teachers {
			departments := teacher.Departments()
			deptList := make([]struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			}, len(departments))

			for i, dept := range departments {
				deptList[i] = struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				}{
					ID:   dept.ID,
					Name: dept.Name,
				}
			}

			teacherResp := TeacherResponse{
				ID:    teacher.ID,
				Name:  teacher.Name,
				Title: teacher.Title,
				Email: func() string {
					email, _ := teacher.Email()
					return email
				}(),
				Phone: func() string {
					phone, _ := teacher.Phone()
					return phone
				}(),
				Departments: deptList,
			}
			response = append(response, teacherResp)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}


}

func handleCheckTeacherAvailability(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        var request struct {
            TeacherID    int    `json:"teacherId"`
            StartTime    string `json:"startTime"`
            EndTime      string `json:"endTime"`
            DepartmentID int    `json:"departmentId"`
        }

        if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
            sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
            return
        }

        startTime, err := time.Parse(time.RFC3339, request.StartTime)
        if err != nil {
            sendErrorResponse(w, "Invalid start time format", http.StatusBadRequest)
            return
        }

        endTime, err := time.Parse(time.RFC3339, request.EndTime)
        if err != nil {
            sendErrorResponse(w, "Invalid end time format", http.StatusBadRequest)
            return
        }

        ctx := context.Background()

        // Check for existing exams in ALL departments where this teacher is involved
        exams, err := client.Exam.FindMany(
            db.Exam.And(
                db.Exam.Or(
                    db.Exam.TeacherInCharge.Where(
                        db.Teacher.ID.Equals(request.TeacherID),
                    ),
                    db.Exam.Supervisors.Some(
                        db.Teacher.ID.Equals(request.TeacherID),
                    ),
                ),
                db.Exam.StartTime.Lt(endTime),
                db.Exam.EndTime.Gt(startTime),
            ),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error checking teacher availability: %v", err)
            sendErrorResponse(w, "Error checking teacher availability", http.StatusInternalServerError)
            return
        }

        // Teacher is available if no conflicting exams found
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "available": len(exams) == 0,
        })
    }
}

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

		ctx := context.Background()

		// Create teacher with all fields
		teacher, err := client.Teacher.CreateOne(
			db.Teacher.Name.Set(teacherData.Name),
			db.Teacher.Title.Set(teacherData.Title),
			db.Teacher.Email.Set(teacherData.Email),
			db.Teacher.Phone.Set(teacherData.Phone),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Error creating teacher: %v", err), http.StatusInternalServerError)
			return
		}

		// Link teacher to department immediately after creation
		_, err = client.Teacher.FindUnique(
			db.Teacher.ID.Equals(teacher.ID),
		).Update(
			db.Teacher.Departments.Link(
				db.Department.ID.Equals(teacherData.DepartmentId),
			),
		).Exec(ctx)

		if err != nil {
			log.Printf("Error linking department: %v", err)
			// Even if department linking fails, we don't want to fail the whole request
		}

		// Fetch updated teacher with departments
		updatedTeacher, err := client.Teacher.FindUnique(
			db.Teacher.ID.Equals(teacher.ID),
		).With(
			db.Teacher.Departments.Fetch(),
		).Exec(ctx)

		if err != nil {
			sendErrorResponse(w, "Error fetching updated teacher", http.StatusInternalServerError)
			return
		}

		// Broadcast update
		broadcastUpdate(hub, TeacherCreate, updatedTeacher, teacherData.DepartmentId)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    updatedTeacher,
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

		existingTeacher, err := client.Teacher.FindFirst(
			db.Teacher.ID.Equals(id),
			db.Teacher.Departments.Some(
				db.Department.ID.Equals(departmentID),
			),
		).Exec(ctx)

		if err != nil || existingTeacher == nil {
			sendErrorResponse(w, "Teacher not found or unauthorized", http.StatusNotFound)
			return
		}

		var teacherData TeacherRequest
		if err := json.NewDecoder(r.Body).Decode(&teacherData); err != nil {
			sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
			return
		}

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

		// Prepare response data
		responseData := map[string]interface{}{
			"id":    teacher.ID,
			"name":  teacher.Name,
			"title": teacher.Title,
			"email": func() string {
				email, _ := teacher.Email()
				return email
			}(),
			"phone": func() string {
				phone, _ := teacher.Phone()
				return phone
			}(),
			"departments": teacher.Departments(),
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
			"departmentId": course.DepartmentID,
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
func handleAddTeacherDepartment(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        var data struct {
            TeacherID    int `json:"teacherId"`
            DepartmentID int `json:"departmentId"`
        }

        if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
            sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
            return
        }

        ctx := context.Background()

        // First update the teacher-department relationship
        _, err := client.Teacher.FindUnique(
            db.Teacher.ID.Equals(data.TeacherID),
        ).Update(
            db.Teacher.Departments.Link(
                db.Department.ID.Equals(data.DepartmentID),
            ),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error adding department: %v", err)
            sendErrorResponse(w, "Error adding department to teacher", http.StatusInternalServerError)
            return
        }

        // Then fetch the updated teacher with departments
        teacherWithDepts, err := client.Teacher.FindUnique(
            db.Teacher.ID.Equals(data.TeacherID),
        ).With(
            db.Teacher.Departments.Fetch(),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error fetching updated teacher: %v", err)
            sendErrorResponse(w, "Error fetching updated teacher data", http.StatusInternalServerError)
            return
        }

        // WebSocket ile güncelleme gönder
        broadcastUpdate(hub, TeacherUpdate, teacherWithDepts, data.DepartmentID)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "data":    teacherWithDepts,
        })
    }
}

func handleRemoveTeacherDepartment(client *db.PrismaClient, hub *Hub) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        var data struct {
            TeacherID    int `json:"teacherId"`
            DepartmentID int `json:"departmentId"`
        }

        if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
            sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
            return
        }

        ctx := context.Background()

        teacher, err := client.Teacher.FindUnique(
            db.Teacher.ID.Equals(data.TeacherID),
        ).Update(
            db.Teacher.Departments.Unlink(
                db.Department.ID.Equals(data.DepartmentID),
            ),
        ).Exec(ctx)

        if err != nil {
            sendErrorResponse(w, fmt.Sprintf("Error removing department: %v", err), http.StatusInternalServerError)
            return
        }

        // Broadcast update
        broadcastUpdate(hub, TeacherUpdate, teacher, data.DepartmentID)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "data":    teacher,
        })
    }
}

func handleGetAllTeachers(client *db.PrismaClient) http.HandlerFunc {
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

        // Fetch all teachers with their departments
        teachers, err := client.Teacher.FindMany().With(
            db.Teacher.Departments.Fetch(),
        ).Exec(ctx)

        if err != nil {
            sendErrorResponse(w, fmt.Sprintf("Error fetching teachers: %v", err), http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "data":    teachers,
        })
    }
}

func handleCheckRoomAvailability(client *db.PrismaClient) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }

        var request struct {
            RoomIDs      []int    `json:"roomIds"`
            StartTime    string   `json:"startTime"`
            EndTime      string   `json:"endTime"`
            DepartmentID int      `json:"departmentId"`
        }

        if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
            sendErrorResponse(w, "Invalid request body", http.StatusBadRequest)
            return
        }

        startTime, err := time.Parse(time.RFC3339, request.StartTime)
        if err != nil {
            sendErrorResponse(w, "Invalid start time format", http.StatusBadRequest)
            return
        }

        endTime, err := time.Parse(time.RFC3339, request.EndTime)
        if err != nil {
            sendErrorResponse(w, "Invalid end time format", http.StatusBadRequest)
            return
        }

        ctx := context.Background()

        // Check for existing exams with the selected rooms
        exams, err := client.Exam.FindMany(
            db.Exam.And(
                db.Exam.ExamRooms.Some(
                    db.ExamRoom.ID.In(request.RoomIDs),
                ),
                db.Exam.StartTime.Lt(endTime),
                db.Exam.EndTime.Gt(startTime),
                db.Exam.DepartmentID.Equals(request.DepartmentID),
            ),
        ).Exec(ctx)

        if err != nil {
            log.Printf("Error checking room availability: %v", err)
            sendErrorResponse(w, "Error checking room availability", http.StatusInternalServerError)
            return
        }

        // Rooms are available if no conflicting exams found
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "available": len(exams) == 0,
        })
    }
}
