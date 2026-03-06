package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
)

// ─── Structs ───────────────────────────────────────────────────────────────

type User struct {
	ID       int    `json:"id"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	Nickname string `json:"nickname"`
	Role     string `json:"role"`
}

type Room struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	OwnerID     int    `json:"owner_id"`
	OwnerNick   string `json:"owner_nick"`
	MoodID      int    `json:"mood_id"`
	MoodName    string `json:"mood_name"`
	MoodEmoji   string `json:"mood_emoji"`
	MoodColor   string `json:"mood_color"`
	VideoID     string `json:"video_id"`
	MemberCount int    `json:"member_count"`
	CreatedAt   string `json:"created_at"`
}

type MoodCategory struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Emoji   string `json:"emoji"`
	VideoID string `json:"video_id"`
	Color   string `json:"color"`
}

type WSMessage struct {
	Type    string          `json:"type"`
	Content string          `json:"content"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Client struct {
	conn     *websocket.Conn
	userID   int
	nickname string
	roomID   int
}

// ─── Globals ───────────────────────────────────────────────────────────────

var (
	db       *sql.DB
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mu       sync.Mutex
	clients  = make(map[*websocket.Conn]*Client)
)

func hashPassword(pw string) string {
	h := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(h[:])
}

// ─── DB Init ───────────────────────────────────────────────────────────────

// ── MySQL Config ── แก้ค่าตรงนี้ให้ตรงกับ XAMPP ของคุณ
const (
	dbUser = "root" // ค่า default ของ XAMPP
	dbPass = ""     // ค่า default ของ XAMPP ไม่มี password
	dbHost = "127.0.0.1"
	dbPort = "3306"
	dbName = "datasync"
)

func initDB() {
	var err error
	dsn := dbUser + ":" + dbPass + "@tcp(" + dbHost + ":" + dbPort + ")/" + dbName + "?charset=utf8mb4&parseTime=True&loc=Local"
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal("เชื่อมต่อ MySQL ไม่ได้: ", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatal("MySQL ไม่ตอบสนอง (เปิด XAMPP แล้วหรือยัง?): ", err)
	}
	log.Println("เชื่อมต่อ MySQL สำเร็จ")

	db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id INT PRIMARY KEY AUTO_INCREMENT,
		full_name VARCHAR(255) NOT NULL,
		email VARCHAR(255) UNIQUE NOT NULL,
		nickname VARCHAR(100) UNIQUE NOT NULL,
		password_hash VARCHAR(64) NOT NULL,
		role VARCHAR(20) DEFAULT 'user',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)

	db.Exec(`CREATE TABLE IF NOT EXISTS mood_categories (
		id INT PRIMARY KEY AUTO_INCREMENT,
		name VARCHAR(100) NOT NULL,
		emoji VARCHAR(20) NOT NULL,
		video_id VARCHAR(50) NOT NULL,
		color VARCHAR(20) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)

	db.Exec(`CREATE TABLE IF NOT EXISTS rooms (
		id INT PRIMARY KEY AUTO_INCREMENT,
		name VARCHAR(255) NOT NULL,
		owner_id INT NOT NULL,
		mood_id INT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE CASCADE,
		FOREIGN KEY(mood_id) REFERENCES mood_categories(id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)

	// Seed admin
	var count int
	db.QueryRow("SELECT COUNT(*) FROM users WHERE role='admin'").Scan(&count)
	if count == 0 {
		db.Exec("INSERT INTO users (full_name, email, nickname, password_hash, role) VALUES (?, ?, ?, ?, ?)",
			"Administrator", "admin@dmin", "Admin", hashPassword("1234"), "admin")
		log.Println("สร้าง Admin แล้ว — email: admin@dmin / password: 1234")
	}

	// Seed default mood categories
	db.QueryRow("SELECT COUNT(*) FROM mood_categories").Scan(&count)
	if count == 0 {
		moods := [][]string{
			{"ตื่นเต้น", "⚡", "U4OMcTKlftQ", "#7f1d1d"},
			{"เหงา", "💧", "Yav8Eek8t6Y", "#1e3a5f"},
			{"ไฟลุก", "🔥", "sh0-9HV99dc", "#7c2d12"},
			{"ตั้งใจเรียน", "📖", "gaNgeXCtaDU", "#064e3b"},
		}
		for _, m := range moods {
			db.Exec("INSERT INTO mood_categories (name, emoji, video_id, color) VALUES (?, ?, ?, ?)",
				m[0], m[1], m[2], m[3])
		}
	}
}

// ─── WebSocket Broadcast ───────────────────────────────────────────────────

func broadcastToRoom(roomID int, payload interface{}) {
	mu.Lock()
	defer mu.Unlock()
	for conn, c := range clients {
		if c.roomID == roomID {
			conn.WriteJSON(payload)
		}
	}
}

func roomMemberCount(roomID int) int {
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, c := range clients {
		if c.roomID == roomID {
			n++
		}
	}
	return n
}

// ─── Main ──────────────────────────────────────────────────────────────────

func main() {
	initDB()
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// ── Auth ──
	r.POST("/api/register", func(c *gin.Context) {
		var body struct {
			FullName string `json:"full_name"`
			Email    string `json:"email"`
			Nickname string `json:"nickname"`
			Password string `json:"password"`
		}
		if err := c.BindJSON(&body); err != nil || body.FullName == "" || body.Email == "" || body.Nickname == "" || body.Password == "" {
			c.JSON(400, gin.H{"error": "ข้อมูลไม่ครบ"})
			return
		}
		stmt, err := db.Prepare("INSERT INTO users (full_name, email, nickname, password_hash) VALUES (?, ?, ?, ?)")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		result, err := stmt.Exec(body.FullName, body.Email, body.Nickname, hashPassword(body.Password))
		if err != nil {
			c.JSON(409, gin.H{"error": "อีเมลหรือนามแฝงนี้ถูกใช้แล้ว"})
			return
		}
		id, _ := result.LastInsertId()
		c.JSON(200, gin.H{"id": id, "nickname": body.Nickname, "full_name": body.FullName, "role": "user"})
	})

	r.POST("/api/login", func(c *gin.Context) {
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := c.BindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "ข้อมูลไม่ครบ"})
			return
		}
		stmt, err := db.Prepare("SELECT id, full_name, nickname, role FROM users WHERE email=? AND password_hash=?")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		var u User
		err = stmt.QueryRow(body.Email, hashPassword(body.Password)).Scan(&u.ID, &u.FullName, &u.Nickname, &u.Role)
		if err != nil {
			c.JSON(401, gin.H{"error": "อีเมลหรือรหัสผ่านไม่ถูกต้อง"})
			return
		}
		c.JSON(200, gin.H{"id": u.ID, "full_name": u.FullName, "nickname": u.Nickname, "role": u.Role})
	})

	// ── Mood Categories ──
	r.GET("/api/moods", func(c *gin.Context) {
		rows, err := db.Query("SELECT id, name, emoji, video_id, color FROM mood_categories ORDER BY id")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var list []MoodCategory
		for rows.Next() {
			var m MoodCategory
			rows.Scan(&m.ID, &m.Name, &m.Emoji, &m.VideoID, &m.Color)
			list = append(list, m)
		}
		if list == nil {
			list = []MoodCategory{}
		}
		c.JSON(200, list)
	})

	r.POST("/api/moods", func(c *gin.Context) {
		var body struct {
			Name    string `json:"name"`
			Emoji   string `json:"emoji"`
			VideoID string `json:"video_id"`
			Color   string `json:"color"`
		}
		if err := c.BindJSON(&body); err != nil || body.Name == "" || body.VideoID == "" {
			c.JSON(400, gin.H{"error": "ข้อมูลไม่ครบ"})
			return
		}
		if body.Emoji == "" {
			body.Emoji = "🎵"
		}
		if body.Color == "" {
			body.Color = "#1a1a2e"
		}
		stmt, err := db.Prepare("INSERT INTO mood_categories (name, emoji, video_id, color) VALUES (?, ?, ?, ?)")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		result, _ := stmt.Exec(body.Name, body.Emoji, body.VideoID, body.Color)
		id, _ := result.LastInsertId()
		c.JSON(200, gin.H{"id": id, "name": body.Name, "emoji": body.Emoji, "video_id": body.VideoID, "color": body.Color})
	})

	r.DELETE("/api/moods/:id", func(c *gin.Context) {
		id := c.Param("id")
		stmt, err := db.Prepare("DELETE FROM mood_categories WHERE id=?")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		stmt.Exec(id)
		c.JSON(200, gin.H{"ok": true})
	})

	// ── Rooms ──
	r.GET("/api/rooms", func(c *gin.Context) {
		rows, err := db.Query(`
			SELECT r.id, r.name, r.owner_id, u.nickname, r.mood_id, m.name, m.emoji, m.color, m.video_id, r.created_at
			FROM rooms r
			JOIN users u ON r.owner_id = u.id
			JOIN mood_categories m ON r.mood_id = m.id
			ORDER BY r.id DESC
		`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var list []Room
		for rows.Next() {
			var ro Room
			rows.Scan(&ro.ID, &ro.Name, &ro.OwnerID, &ro.OwnerNick, &ro.MoodID, &ro.MoodName, &ro.MoodEmoji, &ro.MoodColor, &ro.VideoID, &ro.CreatedAt)
			ro.MemberCount = roomMemberCount(ro.ID)
			list = append(list, ro)
		}
		if list == nil {
			list = []Room{}
		}
		c.JSON(200, list)
	})

	r.POST("/api/rooms", func(c *gin.Context) {
		var body struct {
			Name    string `json:"name"`
			OwnerID int    `json:"owner_id"`
			MoodID  int    `json:"mood_id"`
		}
		if err := c.BindJSON(&body); err != nil || body.Name == "" || body.OwnerID == 0 || body.MoodID == 0 {
			c.JSON(400, gin.H{"error": "ข้อมูลไม่ครบ"})
			return
		}
		stmt, err := db.Prepare("INSERT INTO rooms (name, owner_id, mood_id) VALUES (?, ?, ?)")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		result, _ := stmt.Exec(body.Name, body.OwnerID, body.MoodID)
		id, _ := result.LastInsertId()
		c.JSON(200, gin.H{"id": id})
	})

	r.DELETE("/api/rooms/:id", func(c *gin.Context) {
		id := c.Param("id")
		stmt, err := db.Prepare("DELETE FROM rooms WHERE id=?")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		stmt.Exec(id)
		c.JSON(200, gin.H{"ok": true})
	})

	// ── Admin: Users ──
	r.GET("/api/admin/users", func(c *gin.Context) {
		rows, err := db.Query("SELECT id, full_name, email, nickname, role, created_at FROM users ORDER BY id")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var list []map[string]interface{}
		for rows.Next() {
			var id int
			var fullName, email, nickname, role, createdAt string
			rows.Scan(&id, &fullName, &email, &nickname, &role, &createdAt)
			list = append(list, map[string]interface{}{
				"id": id, "full_name": fullName, "email": email,
				"nickname": nickname, "role": role, "created_at": createdAt,
			})
		}
		if list == nil {
			list = []map[string]interface{}{}
		}
		c.JSON(200, list)
	})

	r.DELETE("/api/admin/users/:id", func(c *gin.Context) {
		id := c.Param("id")
		// prevent deleting admin
		var role string
		db.QueryRow("SELECT role FROM users WHERE id=?", id).Scan(&role)
		if role == "admin" {
			c.JSON(403, gin.H{"error": "ไม่สามารถลบ Admin ได้"})
			return
		}
		stmt, err := db.Prepare("DELETE FROM users WHERE id=?")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer stmt.Close()
		stmt.Exec(id)
		c.JSON(200, gin.H{"ok": true})
	})

	// ── WebSocket ──
	r.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		userIDStr := c.Query("uid")
		nickname := c.Query("nick")
		roomIDStr := c.Query("room")
		userID, _ := strconv.Atoi(userIDStr)
		roomID, _ := strconv.Atoi(roomIDStr)

		client := &Client{conn: conn, userID: userID, nickname: nickname, roomID: roomID}
		mu.Lock()
		clients[conn] = client
		mu.Unlock()

		// Notify room: user joined
		if roomID > 0 {
			mu.Lock()
			var memberList []string
			for _, c := range clients {
				if c.roomID == roomID {
					memberList = append(memberList, c.nickname)
				}
			}
			mu.Unlock()
			broadcastToRoom(roomID, map[string]interface{}{
				"type":    "system",
				"content": nickname + " เข้าร่วมห้อง",
				"count":   roomMemberCount(roomID),
				"members": memberList,
			})
		}

		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				mu.Lock()
				delete(clients, conn)
				mu.Unlock()

				if roomID > 0 {
					// Check if disconnected user is the room owner
					var ownerID int
					db.QueryRow("SELECT owner_id FROM rooms WHERE id=?", roomID).Scan(&ownerID)

					if ownerID == userID {
						// Owner left: notify all then delete room
						broadcastToRoom(roomID, map[string]interface{}{
							"type":    "room_closed",
							"content": nickname + " (เจ้าของห้อง) ออกไปแล้ว ห้องถูกปิด",
						})
						mu.Lock()
						for conn2, c2 := range clients {
							if c2.roomID == roomID {
								conn2.WriteJSON(map[string]interface{}{
									"type":    "room_closed",
									"content": "ห้องถูกปิดโดยเจ้าของ",
								})
								conn2.Close()
								delete(clients, conn2)
							}
						}
						mu.Unlock()
						if stmt, err := db.Prepare("DELETE FROM rooms WHERE id=?"); err == nil {
							stmt.Exec(roomID)
							stmt.Close()
						}
					} else {
						broadcastToRoom(roomID, map[string]interface{}{
							"type":    "system",
							"content": nickname + " ออกจากห้อง",
							"count":   roomMemberCount(roomID),
						})
					}
				}
				break
			}

			switch msg.Type {
			case "chat":
				broadcastToRoom(roomID, map[string]interface{}{
					"type":     "chat",
					"content":  msg.Content,
					"nickname": nickname,
					"time":     time.Now().Format("15:04"),
				})
			case "mood":
				var data map[string]string
				json.Unmarshal(msg.Data, &data)
				broadcastToRoom(roomID, map[string]interface{}{
					"type": "mood",
					"data": data,
					"by":   nickname,
				})
			}
		}
	})

	// ── Serve SPA ──
	r.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(200, htmlTemplate)
	})

	log.Println("MyMood running at http://localhost:8080")
	r.Run(":8080")
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="th">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>MyMood</title>
<link href="https://fonts.googleapis.com/css2?family=Nunito:wght@400;600;700;800;900&family=Noto+Sans+Thai:wght@300;400;600;700;800&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#fff5f9;--s1:#ffffff;--s2:#fff0f6;--s3:#ffe4f0;
  --b1:rgba(255,182,207,0.3);--b2:rgba(255,140,185,0.45);
  --pink:#ff6eb4;--pink2:#ffb3d1;--pink3:#ffd6e8;
  --purple:#c084fc;--purple2:#e9d5ff;
  --peach:#ffb085;--mint:#6ee7b7;
  --red:#fb7185;--amber:#fbbf24;--green:#34d399;
  --text:#5c3d5e;--muted:#c4a0c6;--dark:#3d2040;
  --font:'Nunito',sans-serif;--thai:'Noto Sans Thai',sans-serif;
  --r:18px;--r-sm:12px;--r-pill:999px;
}

body{font-family:var(--thai);background:var(--bg);color:var(--text);min-height:100vh;overflow-x:hidden}

/* ── BACKGROUND BUBBLES ── */
body::before{
  content:'';position:fixed;inset:0;pointer-events:none;z-index:0;
  background:
    radial-gradient(circle at 15% 20%, rgba(255,110,180,0.12) 0%, transparent 40%),
    radial-gradient(circle at 85% 10%, rgba(192,132,252,0.12) 0%, transparent 35%),
    radial-gradient(circle at 50% 80%, rgba(255,176,133,0.1) 0%, transparent 40%),
    radial-gradient(circle at 90% 70%, rgba(110,231,183,0.08) 0%, transparent 30%);
}
.float-bubble{position:fixed;border-radius:50%;pointer-events:none;z-index:0;animation:floatUp linear infinite}
@keyframes floatUp{0%{transform:translateY(110vh) scale(0.4);opacity:0.7}100%{transform:translateY(-10vh) scale(1.1);opacity:0}}

/* ── PAGE SYSTEM ── */
.page{display:none;position:relative;z-index:1;min-height:100vh;height:100vh}
.page.active{display:flex;flex-direction:column;overflow:hidden}

/* ── AUTH PAGES ── */
.auth-center{flex:1;display:flex;align-items:center;justify-content:center;padding:24px}
.auth-card{
  width:460px;max-width:100%;background:var(--s1);
  border:2px solid var(--b2);border-radius:28px;overflow:hidden;
  box-shadow:0 8px 40px rgba(255,110,180,0.18),0 2px 12px rgba(192,132,252,0.1);
  animation:cardIn .35s ease
}
@keyframes cardIn{from{opacity:0;transform:translateY(14px)}to{opacity:1;transform:none}}
.auth-top{
  padding:36px 32px 28px;
  background:linear-gradient(135deg,#ffe0ef 0%,#f3e8ff 100%);
  text-align:center;position:relative;overflow:hidden;
  border-bottom:2px solid var(--b2);
}
.auth-top::before{content:'✿';position:absolute;top:10px;left:18px;font-size:1.4rem;opacity:.4;opacity:.4}
.auth-top::after{content:'✿';position:absolute;top:10px;right:18px;font-size:1.4rem;opacity:.4;opacity:.4}
@keyframes spin{to{transform:rotate(360deg)}}
.auth-logo{
  font-family:var(--font);font-size:2rem;font-weight:900;letter-spacing:3px;
  background:linear-gradient(135deg,var(--pink),var(--purple));
  -webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text;
}
.auth-sub{color:var(--muted);font-size:0.8rem;margin-top:6px;letter-spacing:1px;font-weight:600}
.auth-body{padding:28px 32px;display:flex;flex-direction:column;gap:18px}
.auth-footer{padding:0 32px 28px;text-align:center;font-size:0.85rem;color:var(--muted);font-weight:600}
.auth-footer a{color:var(--pink);cursor:pointer;text-decoration:none;font-weight:700}
.auth-footer a:hover{text-decoration:underline}

.field label{
  display:block;font-size:0.75rem;font-weight:800;color:var(--purple);
  text-transform:uppercase;letter-spacing:1.5px;margin-bottom:7px;font-family:var(--font)
}
.field input,.field select{
  width:100%;padding:12px 16px;
  background:var(--s2);border:2px solid var(--b1);
  border-radius:var(--r-sm);color:var(--text);
  font-family:var(--thai);font-size:0.92rem;outline:none;
  transition:border-color .2s,box-shadow .2s;font-weight:600
}
.field input:focus,.field select:focus{
  border-color:var(--pink);
  box-shadow:0 0 0 4px rgba(255,110,180,0.15)
}
.field input::placeholder{color:var(--muted)}
.field select{cursor:pointer;appearance:none}
.field select option{background:#fff}
.err-msg{
  color:var(--red);font-size:0.82rem;display:none;padding:10px 14px;
  background:rgba(251,113,133,0.08);border:2px solid rgba(251,113,133,0.25);
  border-radius:var(--r-sm);font-weight:600
}

/* ── BUTTONS ── */
.btn{
  display:inline-flex;align-items:center;justify-content:center;gap:7px;
  padding:12px 22px;border:none;border-radius:var(--r-pill);cursor:pointer;
  font-family:var(--thai);font-size:0.92rem;font-weight:700;
  transition:all .18s ease;white-space:nowrap
}
.btn:active{transform:scale(.96)}
.btn-full{width:100%}
.btn-accent{
  background:linear-gradient(135deg,var(--pink),var(--purple));color:#fff;
  box-shadow:0 4px 16px rgba(255,110,180,0.35)
}
.btn-accent:hover{transform:translateY(-2px);box-shadow:0 8px 24px rgba(255,110,180,0.45)}
.btn-ghost{
  background:var(--s2);color:var(--text);
  border:2px solid var(--b2)
}
.btn-ghost:hover{border-color:var(--pink);color:var(--pink);background:var(--pink3)}
.btn-danger{background:rgba(251,113,133,.12);color:var(--red);border:2px solid rgba(251,113,133,.3)}
.btn-danger:hover{background:rgba(251,113,133,.22)}
.btn-purple{background:linear-gradient(135deg,var(--purple),#818cf8);color:#fff;box-shadow:0 4px 16px rgba(192,132,252,.35)}
.btn-purple:hover{transform:translateY(-2px)}
.btn-sm{padding:7px 16px;font-size:0.82rem}
.btn-green{background:linear-gradient(135deg,var(--mint),#34d399);color:#fff;box-shadow:0 4px 12px rgba(110,231,183,.35)}
.btn-green:hover{transform:translateY(-2px)}

/* ── TOPBAR ── */
.topbar{
  height:60px;
  background:rgba(255,255,255,0.85);
  border-bottom:2px solid var(--b1);
  display:flex;align-items:center;padding:0 24px;gap:16px;
  position:sticky;top:0;z-index:100;backdrop-filter:blur(16px);
  box-shadow:0 2px 16px rgba(255,110,180,0.1)
}
.topbar-logo{
  font-family:var(--font);font-weight:900;font-size:1.15rem;flex-shrink:0;
  background:linear-gradient(135deg,var(--pink),var(--purple));
  -webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text;
  letter-spacing:2px;
}
.topbar-logo span{opacity:.6}
.topbar-nav{display:flex;gap:6px;flex:1}
.topbar-right{display:flex;align-items:center;gap:10px;flex-shrink:0}
.user-chip{
  display:flex;align-items:center;gap:8px;padding:6px 14px;
  background:linear-gradient(135deg,#ffe0ef,#f3e8ff);
  border:2px solid var(--b2);border-radius:var(--r-pill);font-size:0.82rem;
}
.user-chip .nick{color:var(--pink);font-weight:800;font-family:var(--font)}
.tab-btn{
  padding:7px 18px;border:2px solid transparent;background:none;
  color:var(--muted);font-family:var(--thai);font-size:0.88rem;font-weight:700;
  cursor:pointer;border-radius:var(--r-pill);transition:all .2s
}
.tab-btn:hover{color:var(--pink);background:var(--pink3)}
.tab-btn.active{
  color:var(--pink);border-color:var(--pink2);
  background:linear-gradient(135deg,rgba(255,110,180,.1),rgba(192,132,252,.08))
}

/* ── MAIN CONTENT ── */
.main{flex:1;padding:24px;max-width:1280px;width:100%;margin:0 auto;overflow:hidden;display:flex;flex-direction:column;min-height:0}
.tab-content{display:none;flex:1;min-height:0;overflow:hidden}
.tab-content.active{display:flex;flex-direction:column}

/* ── LOBBY ── */
.lobby-header{display:flex;align-items:center;justify-content:space-between;margin-bottom:24px;flex-wrap:wrap;gap:12px}
.lobby-title{
  font-family:var(--font);font-size:1.1rem;font-weight:900;
  background:linear-gradient(135deg,var(--pink),var(--purple));
  -webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text;
}
.rooms-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:18px}
.room-card{
  background:var(--s1);border:2px solid var(--b1);border-radius:22px;
  overflow:hidden;transition:all .18s ease;cursor:pointer;
  box-shadow:0 2px 12px rgba(255,110,180,0.07)
}
.room-card:hover{
  border-color:var(--pink2);transform:translateY(-5px);
  box-shadow:0 12px 32px rgba(255,110,180,0.2)
}
.room-card-top{padding:20px;display:flex;align-items:center;gap:14px}
.room-emoji{font-size:2.4rem;line-height:1;filter:drop-shadow(0 2px 6px rgba(255,110,180,.3))}
.room-name{font-weight:800;font-size:1rem;color:var(--dark);font-family:var(--font)}
.room-owner{font-size:0.76rem;color:var(--muted);margin-top:3px;font-weight:600}
.room-card-bottom{
  padding:10px 20px;
  border-top:2px solid var(--b1);
  display:flex;justify-content:space-between;align-items:center;
  background:linear-gradient(90deg,var(--pink3),var(--purple2));
}
.room-mood-tag{font-size:0.76rem;color:var(--pink);font-weight:800;font-family:var(--font)}
.room-members{font-size:0.76rem;color:var(--purple);font-weight:700}
.empty-rooms{text-align:center;padding:64px;color:var(--muted)}
.empty-icon{font-size:3.5rem;margin-bottom:12px}
@keyframes bounce{0%,100%{transform:translateY(0)}50%{transform:translateY(-10px)}}

/* ── ROOM VIEW ── */
.room-view{display:grid;grid-template-columns:1fr 360px;gap:20px;flex:1;min-height:0;overflow:hidden}
@media(max-width:900px){.room-view{grid-template-columns:1fr;height:auto}}
.room-left{display:flex;flex-direction:column;gap:16px;overflow:hidden;min-height:0}
.room-right{display:flex;flex-direction:column;gap:16px;overflow:hidden;min-height:0}

/* ── CARD ── */
.card{
  background:var(--s1);border:2px solid var(--b1);border-radius:22px;overflow:hidden;
  box-shadow:0 2px 12px rgba(255,110,180,0.06)
}
.card-head{
  padding:13px 20px;border-bottom:2px solid var(--b1);
  display:flex;align-items:center;justify-content:space-between;
  background:linear-gradient(90deg,var(--pink3),transparent)
}
.card-head h3{font-family:var(--font);font-size:0.82rem;font-weight:800;color:var(--pink);letter-spacing:.5px}
.card-body{padding:18px}
iframe{width:100%;height:420px;border:none;display:block;border-radius:0}
.now-playing{
  display:flex;align-items:center;gap:14px;padding:16px 20px;
  border-bottom:2px solid var(--b1);
  background:linear-gradient(90deg,rgba(255,110,180,.07),transparent);
  transition:background .8s
}
.np-emoji{font-size:2.2rem}
@keyframes wiggle{0%,100%{transform:rotate(-5deg)}50%{transform:rotate(5deg)}}
.np-mood{font-weight:800;font-size:1.05rem;color:var(--dark);font-family:var(--font)}
.np-sub{font-size:0.7rem;color:var(--muted);font-weight:700;letter-spacing:1.5px;margin-top:2px}
.mood-grid{display:grid;gap:9px}
.mood-btn{
  padding:10px 16px;background:var(--s2);border:2px solid var(--b1);
  border-radius:var(--r-pill);color:var(--text);cursor:pointer;
  font-family:var(--thai);font-size:0.86rem;font-weight:700;
  transition:all .2s cubic-bezier(.34,1.56,.64,1);display:flex;align-items:center;gap:8px
}
.mood-btn:hover{
  transform:translateX(5px) scale(1.02);
  border-color:var(--pink2);color:var(--pink);
  background:linear-gradient(90deg,var(--pink3),var(--s2));
  box-shadow:0 4px 12px rgba(255,110,180,.2)
}
.member-list-box{display:flex;flex-direction:column;gap:7px;max-height:140px;overflow-y:auto}
.member-item{display:flex;align-items:center;gap:8px;font-size:0.86rem;font-weight:600}
.member-dot{
  width:8px;height:8px;background:var(--mint);border-radius:50%;
  box-shadow:0 0 8px var(--mint);animation:pulse 2s ease infinite
}
@keyframes pulse{0%,100%{opacity:1;transform:scale(1)}50%{opacity:.6;transform:scale(.8)}}
.chat-box{
  flex:1;overflow-y:auto;display:flex;flex-direction:column;gap:9px;
  padding:14px;background:var(--bg);height:0;min-height:0
}
.chat-box::-webkit-scrollbar{width:5px}
.chat-box::-webkit-scrollbar-thumb{background:var(--pink2);border-radius:10px}
.chat-msg{display:flex;flex-direction:column;gap:3px}
.chat-msg .meta{font-size:0.71rem;color:var(--muted);display:flex;gap:6px;font-weight:700;flex-wrap:nowrap;white-space:nowrap}
.chat-msg .meta .who{color:var(--pink);font-weight:800}
.chat-msg .bubble{
  background:var(--s1);border:2px solid var(--b1);
  padding:8px 14px;border-radius:18px 18px 18px 4px;
  font-size:0.86rem;width:fit-content;max-width:85%;
  animation:fadeUp .15s ease;font-weight:600;color:var(--dark)
}
.chat-msg.own .bubble{
  background:linear-gradient(135deg,rgba(255,110,180,.15),rgba(192,132,252,.1));
  border-color:var(--pink2);align-self:flex-end;
  border-radius:18px 18px 4px 18px
}
.chat-msg.own{align-items:flex-end}
.chat-msg.own .meta{justify-content:flex-end;order:2}
.chat-msg.own .bubble{order:1}
.chat-msg.own .meta{justify-content:flex-end}
.chat-msg.sys .bubble{
  background:rgba(110,231,183,.1);border-color:rgba(110,231,183,.3);
  color:var(--green);font-size:0.78rem;font-style:italic;border-radius:var(--r-pill)
}
@keyframes fadeUp{from{opacity:0;transform:translateY(5px)}to{opacity:1;transform:none}}
.chat-input-row{display:flex;gap:9px;padding:12px 16px;border-top:2px solid var(--b1)}
.chat-inp{
  flex:1;padding:10px 16px;background:var(--s2);border:2px solid var(--b1);
  border-radius:var(--r-pill);color:var(--text);font-family:var(--thai);
  font-size:0.9rem;outline:none;transition:all .2s;font-weight:600
}
.chat-inp:focus{border-color:var(--pink);box-shadow:0 0 0 4px rgba(255,110,180,.12)}
.chat-inp::placeholder{color:var(--muted)}

/* ── ADMIN ── */
.admin-grid{display:grid;grid-template-columns:1fr 1fr;gap:20px}
@media(max-width:800px){.admin-grid{grid-template-columns:1fr}}
.section-title{
  font-family:var(--font);font-size:0.9rem;font-weight:900;
  color:var(--pink);margin-bottom:16px;letter-spacing:.5px
}
table{width:100%;border-collapse:collapse;font-size:0.86rem}
thead tr{border-bottom:2px solid var(--b2)}
th{
  padding:11px 16px;text-align:left;font-family:var(--font);
  font-size:0.72rem;font-weight:800;color:var(--purple);
  text-transform:uppercase;letter-spacing:1px
}
tbody tr{border-bottom:1px solid var(--b1);transition:background .15s}
tbody tr:hover{background:var(--pink3)}
td{padding:12px 16px;font-weight:600}
.badge{display:inline-block;padding:3px 11px;border-radius:var(--r-pill);font-size:0.74rem;font-weight:800}
.badge-admin{background:rgba(251,113,133,.15);color:var(--red);border:2px solid rgba(251,113,133,.3)}
.badge-user{background:rgba(192,132,252,.15);color:var(--purple);border:2px solid rgba(192,132,252,.3)}
.add-mood-form{display:flex;flex-direction:column;gap:12px;margin-bottom:18px}
.form-row{display:grid;grid-template-columns:1fr 60px 1fr;gap:9px}
@media(max-width:600px){.form-row{grid-template-columns:1fr 1fr}}

/* ── MODAL ── */
.overlay{
  position:fixed;inset:0;background:rgba(92,61,94,.5);
  display:none;align-items:center;justify-content:center;
  z-index:500;backdrop-filter:blur(10px)
}
.overlay.open{display:flex}
.modal{
  background:var(--s1);border:2px solid var(--b2);border-radius:28px;
  padding:32px;width:460px;max-width:92vw;
  animation:modalIn .3s cubic-bezier(.34,1.56,.64,1);
  box-shadow:0 16px 48px rgba(255,110,180,0.2)
}
@keyframes modalIn{from{opacity:0;transform:scale(.88)}to{opacity:1;transform:none}}
.modal-title{
  font-family:var(--font);font-size:1rem;font-weight:900;
  background:linear-gradient(135deg,var(--pink),var(--purple));
  -webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text;
  margin-bottom:22px
}
.modal-fields{display:flex;flex-direction:column;gap:16px;margin-bottom:24px}
.modal-actions{display:flex;gap:10px;justify-content:flex-end}

/* ── TOAST ── */
#toasts{position:fixed;bottom:20px;right:20px;z-index:9999;display:flex;flex-direction:column;gap:9px;pointer-events:none}
.toast{
  padding:12px 18px;background:var(--s1);
  border-left:4px solid var(--pink);border-radius:16px;
  font-size:0.84rem;font-weight:700;display:flex;align-items:center;gap:10px;
  box-shadow:0 6px 24px rgba(255,110,180,.25);min-width:210px;
  animation:toastIn .35s cubic-bezier(.34,1.56,.64,1);color:var(--dark)
}
.toast.ok{border-color:var(--mint)}
.toast.err{border-color:var(--red)}
.toast.warn{border-color:var(--amber)}
.toast.out{animation:toastOut .3s ease forwards}
@keyframes toastIn{from{opacity:0;transform:translateX(20px) scale(.9)}to{opacity:1;transform:none}}
@keyframes toastOut{to{opacity:0;transform:translateX(20px)}}

/* ── EMOJI PARTICLES ── */
.ep{position:fixed;pointer-events:none;font-size:2rem;animation:epFly 2.2s ease-out forwards;z-index:99}
@keyframes epFly{0%{transform:translateY(85vh) scale(.4);opacity:1}100%{transform:translateY(-10vh) scale(1.3);opacity:0}}

::-webkit-scrollbar{width:6px;height:6px}
::-webkit-scrollbar-thumb{background:var(--pink2);border-radius:10px}
</style>
</head>
<body>

<!-- Floating Bubbles -->
<div id="bubbles"></div>

<!-- ════ PAGE: LOGIN ════ -->
<div id="page-login" class="page active">
  <div class="auth-center">
    <div class="auth-card">
      <div class="auth-top">
        <div class="auth-logo">🌸 MyMood</div>
        <div class="auth-sub">✨ ระบบห้องเพลง ✨</div>
      </div>
      <div class="auth-body">
        <div class="err-msg" id="login-err"></div>
        <div class="field"><label>อีเมล</label><input type="email" id="li-email" placeholder="your@email.com" autocomplete="off" onkeydown="if(event.key==='Enter')doLogin()"></div>
        <div class="field"><label>รหัสผ่าน</label><input type="password" id="li-pass" placeholder="••••••" autocomplete="new-password" onkeydown="if(event.key==='Enter')doLogin()"></div>
        <button class="btn btn-accent btn-full" onclick="doLogin()">เข้าสู่ระบบ →</button>
      </div>
      <div class="auth-footer">ยังไม่มีบัญชี? <a onclick="showPage('page-register')">สมัครสมาชิก</a></div>
    </div>
  </div>
</div>

<!-- ════ PAGE: REGISTER ════ -->
<div id="page-register" class="page">
  <div class="auth-center">
    <div class="auth-card">
      <div class="auth-top">
        <div class="auth-logo">🌸 MyMood</div>
        <div class="auth-sub">💕 สร้างบัญชีใหม่</div>
      </div>
      <div class="auth-body">
        <div class="err-msg" id="reg-err"></div>
        <div class="field"><label>ชื่อ-นามสกุลจริง</label><input type="text" id="rg-name" placeholder="ชื่อ นามสกุล" autocomplete="off"></div>
        <div class="field"><label>อีเมล</label><input type="email" id="rg-email" placeholder="your@email.com" autocomplete="off"></div>
        <div class="field"><label>นามแฝง (ชื่อที่จะแสดงในห้อง)</label><input type="text" id="rg-nick" placeholder="Nickname" autocomplete="off"></div>
        <div class="field"><label>ตั้งรหัสผ่าน</label><input type="password" id="rg-pass" placeholder="อย่างน้อย 4 ตัวอักษร" autocomplete="new-password"></div>
        <button class="btn btn-accent btn-full" onclick="doRegister()">สมัครสมาชิก →</button>
      </div>
      <div class="auth-footer">มีบัญชีแล้ว? <a onclick="showPage('page-login')">เข้าสู่ระบบ</a></div>
    </div>
  </div>
</div>

<!-- ════ PAGE: APP ════ -->
<div id="page-app" class="page">
  <!-- Topbar -->
  <div class="topbar">
    <div class="topbar-logo">🌸 MyMood<span>SYNC</span></div>
    <div class="topbar-nav" id="app-nav">
      <button class="tab-btn active" onclick="switchTab('tab-lobby',this)">🏠 ห้องทั้งหมด</button>
      <button class="tab-btn" id="admin-btn" style="display:none" onclick="switchTab('tab-admin',this)">⚙ Admin</button>
    </div>
    <div class="topbar-right">
      <div class="user-chip"><span class="nick" id="user-nick-display"></span></div>
      <button class="btn btn-ghost btn-sm" onclick="doLogout()">ออก</button>
    </div>
  </div>

  <div class="main">

    <!-- ── TAB: LOBBY ── -->
    <div id="tab-lobby" class="tab-content active">
      <div class="lobby-header">
        <div class="lobby-title">🎀 ห้องเพลงทั้งหมด</div>
        <button class="btn btn-accent" onclick="openCreateRoom()">🌸 สร้างห้องใหม่</button>
      </div>
      <div id="rooms-grid" class="rooms-grid"></div>
      <div id="rooms-empty" class="empty-rooms" style="display:none">
        <div class="empty-icon">🎵</div>
        <div style="font-weight:700">ยังไม่มีห้องเลย 🥺<br><span style="font-size:.85rem">เป็นคนแรกที่สร้างห้องซิ!</span></div>
      </div>
    </div>

    <!-- ── TAB: ROOM ── -->
    <div id="tab-room" class="tab-content">
      <div style="display:flex;align-items:center;gap:12px;margin-bottom:18px;flex-wrap:wrap">
        <button class="btn btn-ghost btn-sm" onclick="leaveRoom()">← กลับ</button>
        <div style="font-family:var(--mono);font-size:0.8rem;letter-spacing:2px;color:var(--accent)" id="room-title-bar">ROOM</div>
        <div style="font-size:0.75rem;color:var(--muted)" id="room-member-bar">0 คน</div>
      </div>
      <div class="room-view">
        <div class="room-left">
          <div class="card">
            <div class="now-playing" id="now-playing">
              <div class="np-emoji" id="np-emoji">🎵</div>
              <div><div class="np-mood" id="np-mood">ผ่อนคลาย</div><div class="np-sub">♪ NOW PLAYING ♪</div></div>
            </div>
            <iframe id="yt-player" src="" allow="autoplay;encrypted-media" allowfullscreen></iframe>
          </div>

        </div>
        <div class="room-right">
          <div class="card">
            <div class="card-head"><h3>👥 สมาชิกในห้อง</h3></div>
            <div class="card-body" style="padding:12px">
              <div class="member-list-box" id="member-list-box"></div>
            </div>
          </div>
          <div class="card" style="flex:1;display:flex;flex-direction:column;overflow:hidden;min-height:0">
            <div class="card-head"><h3>💬 Chat</h3></div>
            <div class="chat-box" id="chat-box"></div>
            <div class="chat-input-row">
              <input type="text" class="chat-inp" id="chat-inp" placeholder="พิมพ์ข้อความ..." onkeydown="if(event.key==='Enter')sendChat()">
              <button class="btn btn-accent btn-sm" onclick="sendChat()">ส่ง</button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- ── TAB: ADMIN ── -->
    <div id="tab-admin" class="tab-content">
      <div class="admin-grid">
        <div>
          <div class="section-title">💜 จัดการผู้ใช้</div>
          <div class="card">
            <div class="card-body" style="padding:0">
              <table>
                <thead><tr><th>ID</th><th>นามแฝง</th><th>อีเมล</th><th>Role</th><th></th></tr></thead>
                <tbody id="admin-users-list"></tbody>
              </table>
            </div>
          </div>
        </div>
        <div>
          <div class="section-title">🎵 หมวด Mood</div>
          <div class="card">
            <div class="card-body">
              <div class="add-mood-form">
                <div class="form-row">
                  <div class="field"><label>ชื่อหมวด</label><input type="text" id="m-name" class="chat-inp" style="width:100%" placeholder="เช่น สงบ"></div>
                  <div class="field"><label>Emoji</label><input type="text" id="m-emoji" class="chat-inp" style="width:100%" placeholder="🎵"></div>
                  <div class="field"><label>YouTube Video ID</label><input type="text" id="m-vid" class="chat-inp" style="width:100%" placeholder="dQw4w9WgXcQ"></div>
                </div>
                <div class="field"><label>สีพื้นหลัง (hex)</label><input type="text" id="m-color" class="chat-inp" style="width:100%" placeholder="#1a1a2e"></div>
                <button class="btn btn-accent btn-sm" onclick="addMoodCategory()">+ เพิ่มหมวด</button>
              </div>
              <table>
                <thead><tr><th>Emoji</th><th>ชื่อ</th><th>Video</th><th></th></tr></thead>
                <tbody id="admin-moods-list"></tbody>
              </table>
            </div>
          </div>
        </div>
      </div>
    </div>

  </div>
</div>

<!-- MODAL: CREATE ROOM -->
<div class="overlay" id="modal-create-room">
  <div class="modal">
    <div class="modal-title">🌸 สร้างห้องใหม่</div>
    <div class="modal-fields">
      <div class="field"><label>ชื่อห้อง</label><input type="text" id="cr-name" class="chat-inp" style="width:100%" placeholder="เช่น ห้องเรียนยามเช้า"></div>
      <div class="field"><label>เลือก Mood เริ่มต้น</label><select id="cr-mood" class="chat-inp" style="width:100%;padding:11px 14px;appearance:none"></select></div>
    </div>
    <div class="modal-actions">
      <button class="btn btn-ghost" onclick="closeModal('modal-create-room')">ยกเลิก</button>
      <button class="btn btn-accent" onclick="createRoom()">สร้างห้อง →</button>
    </div>
  </div>
</div>

<div id="toasts"></div>

<script>
// ─── State ───
var SESSION = null; // {id, full_name, nickname, role}
var WS = null;
var currentRoom = null; // {id, name}
var currentMembers = [];
var allMoods = [];

// ─── Page Router ───
function showPage(id) {
  document.querySelectorAll('.page').forEach(function(p){p.classList.remove('active')});
  document.getElementById(id).classList.add('active');
}

// ─── Auth ───
function doLogin() {
  var email = document.getElementById('li-email').value.trim();
  var pass  = document.getElementById('li-pass').value;
  if (!email || !pass) { showErr('login-err','กรุณากรอกข้อมูลให้ครบ'); return; }
  fetch('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({email:email,password:pass})})
    .then(function(r){return r.json()})
    .then(function(d){
      if (d.error) { showErr('login-err', d.error); return; }
      SESSION = d;
      sessionStorage.setItem('datasync_session', JSON.stringify(d));
      afterLogin();
    }).catch(function(){showErr('login-err','เกิดข้อผิดพลาด')});
}

function doRegister() {
  var name  = document.getElementById('rg-name').value.trim();
  var email = document.getElementById('rg-email').value.trim();
  var nick  = document.getElementById('rg-nick').value.trim();
  var pass  = document.getElementById('rg-pass').value;
  if (!name||!email||!nick||!pass) { showErr('reg-err','กรุณากรอกข้อมูลให้ครบ'); return; }
  if (pass.length < 4) { showErr('reg-err','รหัสผ่านต้องมีอย่างน้อย 4 ตัวอักษร'); return; }
  fetch('/api/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({full_name:name,email:email,nickname:nick,password:pass})})
    .then(function(r){return r.json()})
    .then(function(d){
      if (d.error) { showErr('reg-err', d.error); return; }
      SESSION = d;
      sessionStorage.setItem('datasync_session', JSON.stringify(d));
      afterLogin();
      toast('ยินดีต้อนรับ ' + nick + ' !','ok');
    }).catch(function(){showErr('reg-err','เกิดข้อผิดพลาด')});
}

function afterLogin() {
  document.getElementById('user-nick-display').textContent = SESSION.nickname;
  if (SESSION.role === 'admin') document.getElementById('admin-btn').style.display = '';
  else document.getElementById('admin-btn').style.display = 'none';
  showPage('page-app');
  loadMoods(function(){ loadRooms(); });
}

function doLogout() {
  if (WS) { WS.close(); WS = null; }
  SESSION = null; currentRoom = null;
  sessionStorage.removeItem('datasync_session');
  showPage('page-login');
}

// ─── Restore session เมื่อ F5 / reload ───
(function() {
  var saved = sessionStorage.getItem('datasync_session');
  if (saved) {
    try {
      SESSION = JSON.parse(saved);
      afterLogin();
    } catch(e) {
      sessionStorage.removeItem('datasync_session');
    }
  }
})();

// ─── Tab ───
function switchTab(id, btn) {
  document.querySelectorAll('.tab-content').forEach(function(t){t.classList.remove('active')});
  document.querySelectorAll('.tab-btn').forEach(function(b){b.classList.remove('active')});
  document.getElementById(id).classList.add('active');
  btn.classList.add('active');
  if (id==='tab-admin') { loadAdminUsers(); loadAdminMoods(); }
  if (id==='tab-lobby') loadRooms();
}

// ─── Moods ───
function loadMoods(cb) {
  fetch('/api/moods').then(function(r){return r.json()}).then(function(d){
    allMoods = d;
    if (cb) cb();
  });
}

// ─── Rooms ───
function loadRooms() {
  fetch('/api/rooms').then(function(r){return r.json()}).then(function(rooms){
    var grid = document.getElementById('rooms-grid');
    var empty = document.getElementById('rooms-empty');
    if (!rooms.length) { grid.innerHTML=''; empty.style.display='block'; return; }
    empty.style.display='none';
    grid.innerHTML = rooms.map(function(r){
      return '<div class="room-card" onclick="joinRoom('+r.id+',\''+esc(r.name)+'\',\''+esc(r.video_id)+'\',\''+esc(r.mood_name)+'\',\''+esc(r.mood_emoji)+'\',\''+esc(r.mood_color || '')+'\')">' +
        '<div class="room-card-top">' +
          '<div class="room-emoji">'+r.mood_emoji+'</div>' +
          '<div><div class="room-name">'+esc(r.name)+'</div><div class="room-owner">โดย '+esc(r.owner_nick)+'</div></div>' +
        '</div>' +
        '<div class="room-card-bottom">' +
          '<span class="room-mood-tag">'+esc(r.mood_name)+'</span>' +
          '<span class="room-members">👥 '+(r.member_count||0)+' คน</span>' +
        '</div>' +
      '</div>';
    }).join('');
  });
}

function openCreateRoom() {
  var sel = document.getElementById('cr-mood');
  sel.innerHTML = allMoods.map(function(m){
    return '<option value="'+m.id+'">'+m.emoji+' '+m.name+'</option>';
  }).join('');
  openModal('modal-create-room');
}

function createRoom() {
  var name   = document.getElementById('cr-name').value.trim();
  var moodID = parseInt(document.getElementById('cr-mood').value);
  if (!name) { toast('กรุณาตั้งชื่อห้อง','warn'); return; }
  fetch('/api/rooms',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:name,owner_id:SESSION.id,mood_id:moodID})})
    .then(function(r){return r.json()})
    .then(function(d){
      if (d.error) { toast(d.error,'err'); return; }
      closeModal('modal-create-room');
      document.getElementById('cr-name').value='';
      var mood = allMoods.find(function(m){return m.id===moodID});
      joinRoom(d.id, name, mood?mood.video_id:'', mood?mood.name:'', mood?mood.emoji:'🎵', mood?mood.color:'');
    });
}

// ─── Join Room ───
function joinRoom(roomID, roomName, videoID, moodName, moodEmoji, moodColor) {
  currentRoom = {id:roomID, name:roomName};
  currentMembers = [];

  document.getElementById('room-title-bar').textContent = roomName;
  document.getElementById('np-mood').textContent = moodName;
  document.getElementById('np-emoji').textContent = moodEmoji;
  document.getElementById('yt-player').src = 'https://www.youtube.com/embed/'+videoID+'?autoplay=1';
  document.getElementById('now-playing').style.background = 'linear-gradient(90deg,'+(moodColor||'#1a1a2e')+'55,transparent)';
  document.getElementById('chat-box').innerHTML = '';
  document.getElementById('member-list-box').innerHTML = '';

  // Navigate
  document.querySelectorAll('.tab-content').forEach(function(t){t.classList.remove('active')});
  document.querySelectorAll('.tab-btn').forEach(function(b){b.classList.remove('active')});
  document.getElementById('tab-room').classList.add('active');

  // Connect WS
  if (WS) WS.close();
  WS = new WebSocket('ws://'+window.location.host+'/ws?uid='+SESSION.id+'&nick='+encodeURIComponent(SESSION.nickname)+'&room='+roomID);
  WS.onmessage = function(e){
    var msg = JSON.parse(e.data);
    if (msg.type==='chat') { addChatMsg(msg.nickname, msg.content, msg.time, msg.nickname===SESSION.nickname); }
    else if (msg.type==='system') {
      addSysMsg(msg.content);
      updateMemberCount(msg.count);
      if (msg.members) updateMemberList(msg.members);
    }
    else if (msg.type==='room_closed') {
      toast(msg.content || 'ห้องถูกปิดแล้ว', 'warn');
      if (WS) { WS.onclose = null; WS.close(); WS = null; }
      currentRoom = null;
      document.getElementById('yt-player').src = '';
      document.querySelectorAll('.tab-content').forEach(function(t){t.classList.remove('active')});
      document.querySelectorAll('.tab-btn').forEach(function(b){b.classList.remove('active')});
      document.getElementById('tab-lobby').classList.add('active');
      document.querySelector('.tab-btn').classList.add('active');
      loadRooms();
    }
    else if (msg.type==='mood') { applyMood(msg.data); }
  };
  WS.onclose = function(){ if(currentRoom) toast('การเชื่อมต่อหลุด...','warn'); };
}

function leaveRoom() {
  if (WS) { WS.close(); WS = null; }
  currentRoom = null;
  document.getElementById('yt-player').src='';
  switchTab('tab-lobby', document.querySelector('.tab-btn'));
  loadRooms();
}

function sendMoodWS(moodID) {
  if (!WS || WS.readyState!==1) return;
  var mood = allMoods.find(function(m){return m.id===moodID});
  if (!mood) return;
  WS.send(JSON.stringify({type:'mood',data:JSON.stringify({video_id:mood.video_id,mood_name:mood.name,emoji:mood.emoji,color:mood.color})}));
}

function applyMood(data) {
  document.getElementById('np-mood').textContent = data.mood_name;
  document.getElementById('np-emoji').textContent = data.emoji;
  document.getElementById('yt-player').src = 'https://www.youtube.com/embed/'+data.video_id+'?autoplay=1';
  document.getElementById('now-playing').style.background = 'linear-gradient(90deg,'+data.color+'55,transparent)';
  spawnEmoji(data.emoji);
}

// ─── Chat ───
function sendChat() {
  var inp = document.getElementById('chat-inp');
  var txt = inp.value.trim();
  if (!txt||!WS||WS.readyState!==1) return;
  WS.send(JSON.stringify({type:'chat',content:txt}));
  inp.value='';
}
function addChatMsg(who, txt, time, own) {
  var box = document.getElementById('chat-box');
  var d = document.createElement('div');
  d.className = 'chat-msg'+(own?' own':'');
  if (own) {
    d.innerHTML = '<div class="bubble">'+esc(txt)+'</div><div class="meta"><span class="who">'+esc(who)+'</span><span>'+time+'</span></div>';
  } else {
    d.innerHTML = '<div class="meta"><span class="who">'+esc(who)+'</span><span>'+time+'</span></div><div class="bubble">'+esc(txt)+'</div>';
  }
  box.appendChild(d);
  box.scrollTop = box.scrollHeight;
}
function addSysMsg(txt) {
  var box = document.getElementById('chat-box');
  var d = document.createElement('div');
  d.className = 'chat-msg sys';
  d.innerHTML = '<div class="bubble">'+esc(txt)+'</div>';
  box.appendChild(d);
  box.scrollTop = box.scrollHeight;
}
function updateMemberList(members) {
  var box = document.getElementById('member-list-box');
  box.innerHTML = members.map(function(nick){
    return '<div class="member-item"><div class="member-dot"></div><span>'+esc(nick)+'</span></div>';
  }).join('');
}
function updateMemberCount(n) {
  document.getElementById('room-member-bar').textContent = (n||0)+' คนออนไลน์';
}

// ─── Admin: Users ───
function loadAdminUsers() {
  fetch('/api/admin/users').then(function(r){return r.json()}).then(function(users){
    document.getElementById('admin-users-list').innerHTML = users.map(function(u){
      var del = u.role!=='admin'
        ? '<button class="btn btn-danger btn-sm" onclick="deleteUser('+u.id+',\''+esc(u.nickname)+'\')">ลบ</button>'
        : '<span style="font-size:.75rem;color:var(--muted)">—</span>';
      return '<tr><td style="font-family:var(--mono);color:var(--muted)">#'+u.id+'</td><td>'+esc(u.nickname)+'</td><td style="font-size:.8rem;color:var(--muted)">'+esc(u.email)+'</td><td><span class="badge badge-'+u.role+'">'+u.role+'</span></td><td>'+del+'</td></tr>';
    }).join('');
  });
}
function deleteUser(id, nick) {
  if (!confirm('ลบผู้ใช้ "'+nick+'" ออกจากระบบ?')) return;
  fetch('/api/admin/users/'+id,{method:'DELETE'}).then(function(r){return r.json()}).then(function(d){
    if (d.error) { toast(d.error,'err'); return; }
    toast('ลบผู้ใช้ '+nick+' แล้ว','warn');
    loadAdminUsers();
  });
}

// ─── Admin: Moods ───
function loadAdminMoods() {
  fetch('/api/moods').then(function(r){return r.json()}).then(function(moods){
    allMoods = moods;
    document.getElementById('admin-moods-list').innerHTML = moods.map(function(m){
      return '<tr><td>'+m.emoji+'</td><td>'+esc(m.name)+'</td><td style="font-family:var(--mono);font-size:.75rem;color:var(--muted)">'+esc(m.video_id)+'</td><td><button class="btn btn-danger btn-sm" onclick="deleteMood('+m.id+')">ลบ</button></td></tr>';
    }).join('');
  });
}
function addMoodCategory() {
  var name  = document.getElementById('m-name').value.trim();
  var emoji = document.getElementById('m-emoji').value.trim()||'🎵';
  var vid   = document.getElementById('m-vid').value.trim();
  var color = document.getElementById('m-color').value.trim()||'#1a1a2e';
  if (!name||!vid) { toast('กรุณากรอกชื่อและ Video ID','warn'); return; }
  fetch('/api/moods',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:name,emoji:emoji,video_id:vid,color:color})})
    .then(function(r){return r.json()})
    .then(function(d){
      if (d.error) { toast(d.error,'err'); return; }
      document.getElementById('m-name').value='';
      document.getElementById('m-emoji').value='';
      document.getElementById('m-vid').value='';
      document.getElementById('m-color').value='';
      toast('เพิ่มหมวด "'+name+'" แล้ว','ok');
      loadAdminMoods();
    });
}
function deleteMood(id) {
  if (!confirm('ลบหมวดนี้?')) return;
  fetch('/api/moods/'+id,{method:'DELETE'}).then(function(){
    toast('ลบหมวดแล้ว','warn');
    loadAdminMoods();
  });
}

// ─── Modal ───
function openModal(id) { document.getElementById(id).classList.add('open'); }
function closeModal(id) { document.getElementById(id).classList.remove('open'); }
document.querySelectorAll('.overlay').forEach(function(o){
  o.addEventListener('click',function(e){ if(e.target===this) this.classList.remove('open'); });
});

// ─── Toast ───
function toast(msg, type) {
  var c = document.getElementById('toasts');
  var t = document.createElement('div');
  var icons = {ok:'✓',err:'✗',warn:'⚠'};
  t.className = 'toast '+(type||'');
  t.innerHTML = '<span>'+(icons[type]||'ℹ')+'</span>'+msg;
  c.appendChild(t);
  setTimeout(function(){
    t.classList.add('out');
    setTimeout(function(){t.remove()},300);
  },3000);
}

// ─── Emoji ───
function spawnEmoji(e) {
  for (var i=0;i<7;i++){
    var d=document.createElement('div');
    d.className='ep'; d.textContent=e;
    d.style.left=(Math.random()*94)+'vw';
    d.style.animationDelay=(Math.random()*.6)+'s';
    document.body.appendChild(d);
    setTimeout(function(el){return function(){el.remove()}}(d),2800);
  }
}

// ─── Utils ───

// ─── Floating Bubbles ───
(function() {
  var colors = ['rgba(255,110,180,0.25)','rgba(192,132,252,0.22)','rgba(255,176,211,0.3)','rgba(255,209,232,0.35)','rgba(110,231,183,0.2)'];
  var container = document.getElementById('bubbles');
  for (var i = 0; i < 5; i++) {
    (function(i) {
      var b = document.createElement('div');
      b.className = 'float-bubble';
      var size = 18 + Math.random() * 44;
      b.style.cssText = [
        'width:'+size+'px','height:'+size+'px',
        'left:'+(Math.random()*96)+'vw',
        'background:'+colors[Math.floor(Math.random()*colors.length)],
        'animation-duration:'+(9+Math.random()*14)+'s',
        'animation-delay:'+(Math.random()*12)+'s',
        'border:1.5px solid rgba(255,140,185,0.3)'
      ].join(';');
      container.appendChild(b);
    })(i);
  }
})();

function esc(s) {
  return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
function showErr(id, msg) {
  var el = document.getElementById(id);
  el.textContent = msg; el.style.display='block';
  setTimeout(function(){el.style.display='none'},4000);
}
</script>
</body>
</html>`
