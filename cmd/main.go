package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	// "os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	xStart   = 2200
	yStart   = 1300
	fps      = 20
	interval = 1000 / fps
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(req *http.Request) bool { return true },
	}

	playerStateBroadcast = make(chan *Player)
	clientInfoBroadcast  = make(chan *ClientConnectionPayload)
	diningInfoBroadcast  = make(chan *DiningStatePayload)
	ferrisInfoBroadcast  = make(chan *FerrisStatePayload)
	benchInfoBroadcast   = make(chan *BenchStatePayload)
	startFirework        = make(chan bool)

	playerList  = newPlayerList()
	ferrisState = &FerrisState{players: []string{}}
)

type PlayerList struct {
	pList map[*Player]bool

	mu sync.Mutex
}

type Player struct {
	conn  *websocket.Conn
	id    string
	state *State

	mu sync.Mutex
}

type State struct {
	Color       string `json:"color"`
	Action      string `json:"action"`
	Target      string `json:"target"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Frame       int    `json:"frame"`
	ChangeFrame bool   `json:"changeFrame"`
	Facing      string `json:"facing"`
}

type StateMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type PlayerMessage struct {
	Color       string `json:"color"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Facing      string `json:"facing"`
	Frame       int    `json:"frame"`
	ChangeFrame bool   `json:"changeFrame"`
	Action      string `json:"action"`
}

type DiningMessage struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type FerrisMessage struct {
	Action string `json:"action"`
	Player string `json:"player"`
}

type BenchMessage struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type ClientConnectionPayload struct {
	Type string `json:"type"`
	Id   string `json:"id"`
}

type PlayerStatePayload struct {
	Type  string `json:"type"`
	Id    string `json:"id"`
	State *State `json:"state"`
}

type DiningStatePayload struct {
	Type  string `json:"type"`
	Left  string `json:"left"`
	Right string `json:"right"`
}

type FerrisStatePayload struct {
	Type    string   `json:"type"`
	Frame   int      `json:"frame"`
	Players []string `json:"players"`
}

type BenchStatePayload struct {
	Type          string `json:"type"`
	Left          string `json:"left"`
	Right         string `json:"right"`
	ShowFireworks bool   `json:"showFireworks"`
}

type FerrisState struct {
	players []string
	frame   int
	cycle   int

	mu sync.Mutex
}

type LoginRequest struct {
	Password string `json:"password"`
}

type ResponseData struct {
	Status bool `json:"status"`
}

func main() {
	mux := http.NewServeMux()
	// mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/ws", handleConnections)

	go handleMoveBroadcast()
	go handleDisconnectBroadcast()
	go handleDiningBroadcast()
	go handleFerrisBroadcast()
	go handleFerrisState()
	go handleBenchBroadcast()

	log.Fatal(http.ListenAndServe(":8080", corsMiddleware(mux)))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, req)
	})
}

// func handleLogin(w http.ResponseWriter, req *http.Request) {
// 	if req.Method == http.MethodPost {
// 		var loginRequest LoginRequest
// 		err := json.NewDecoder(req.Body).Decode(&loginRequest)
// 		if err != nil {
// 			http.Error(w, "invalid request bode", http.StatusBadRequest)
// 		}
//
// 		response := &ResponseData{}
// 		if loginRequest.Password == os.Getenv("PASSWORD") {
// 			response.Status = true
// 		}
//
// 		w.Header().Set("Content-Type", "application/json")
// 		w.WriteHeader(http.StatusOK)
// 		json.NewEncoder(w).Encode(response)
// 	}
// }

func newPlayerList() *PlayerList {
	return &PlayerList{pList: make(map[*Player]bool)}
}

func newPlayer(conn *websocket.Conn) *Player {
	return &Player{
		conn: conn,
		id:   fmt.Sprintf("player_%d", len(playerList.pList)+1),
		state: &State{
			Action: "idle",
			X:      xStart,
			Y:      yStart,
			Facing: "left",
		},
	}
}

// initiate newly connected player
func handleConnections(w http.ResponseWriter, req *http.Request) {
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("failed to upgrade to websocket:\n%v\n", err)
		return
	}

	player := newPlayer(conn)
	playerList.mu.Lock()
	playerList.pList[player] = true
	playerList.mu.Unlock()
	log.Printf("%v connected\n", player.id)

	playerStatePayload := &PlayerStatePayload{Type: "playerState", Id: player.id, State: player.state}
	clientInfoPayload := &ClientConnectionPayload{Type: "connected", Id: player.id}
	player.mu.Lock()
	player.conn.WriteJSON(*playerStatePayload)
	player.conn.WriteJSON(*clientInfoPayload)
	player.mu.Unlock()

	go handleMessage(player)
}

func handleMessage(player *Player) {
	defer func() {
		player.conn.Close()

		playerList.mu.Lock()
		delete(playerList.pList, player)
		playerList.mu.Unlock()

		clientInfoPayload := &ClientConnectionPayload{Type: "disconnected", Id: player.id}
		clientInfoBroadcast <- clientInfoPayload

		log.Printf("%v disconnected.", player.id)
	}()

	playerStateBroadcast <- player
	for p := range playerList.pList {
		payload := &PlayerStatePayload{Type: "playerState", Id: p.id, State: p.state}
		player.mu.Lock()
		err := player.conn.WriteJSON(*payload)
		player.mu.Unlock()

		if err != nil {
			log.Printf("Failed to update %v's state on %v\n", p.id, player.id)
		}
	}

	for {
		var message StateMessage
		err := player.conn.ReadJSON(&message)
		if err != nil {
			log.Printf("error reading player action:\n%v\n", err)
			return
		}

		switch message.Type {
		case "player":
			databytes, err := json.Marshal(message.Data)
			if err != nil {
				log.Printf("failed to encode json on player message data:\n%v\n", err)
			}

			var playerMessage PlayerMessage
			err = json.Unmarshal(databytes, &playerMessage)
			if err != nil {
				log.Printf("failed to decode player message data:\n%v\n", err)
			}

			player.mu.Lock()
			if playerMessage.Color != "" {
				player.state.Color = playerMessage.Color
			}
			player.state.X = playerMessage.X
			player.state.Y = playerMessage.Y
			player.state.Facing = playerMessage.Facing
			player.state.Frame = playerMessage.Frame
			player.state.ChangeFrame = playerMessage.ChangeFrame
			player.state.Action = playerMessage.Action
			player.mu.Unlock()

			playerStateBroadcast <- player

		case "dining":
			databytes, err := json.Marshal(message.Data)
			if err != nil {
				log.Printf("failed to encode on dining data:\n%v\n", err)
			}

			var diningMessage DiningMessage
			err = json.Unmarshal(databytes, &diningMessage)
			if err != nil {
				log.Printf("failed to decode dining message data:\n%v\n", err)
			}

			diningStatePayload := &DiningStatePayload{
				Type:  "diningState",
				Left:  diningMessage.Left,
				Right: diningMessage.Right,
			}

			diningInfoBroadcast <- diningStatePayload

		case "ferris":
			databytes, err := json.Marshal(message.Data)
			if err != nil {
				log.Printf("failed to encode json on ferris data:\n%v\n", err)
			}

			var ferrisMessage FerrisMessage
			err = json.Unmarshal(databytes, &ferrisMessage)
			if err != nil {
				log.Printf("failed to decode json on ferris message:\n%v\n", err)
			}
			switch ferrisMessage.Action {
			case "join":
				ferrisState.mu.Lock()
				ferrisState.players = append(ferrisState.players, ferrisMessage.Player)
				ferrisState.mu.Unlock()
			case "cancel":
				idx := -1
				for i, p := range ferrisState.players {
					if p == ferrisMessage.Player {
						idx = i
					}
				}
				if idx > -1 {
					ferrisState.mu.Lock()
					ferrisState.players = append(ferrisState.players[:idx], ferrisState.players[idx+1:]...)
					ferrisState.mu.Unlock()
				}

			case "exit":
				for p := range playerList.pList {
					p.state.Action = "idle"
					playerStateBroadcast <- p
				}
				ferrisState.players = []string{}
			}

		case "bench":
			databytes, err := json.Marshal(&message.Data)
			if err != nil {
				log.Printf("failed to encode bench info data to json:\n%v\n", err)
			}

			var benchMessage *BenchMessage
			err = json.Unmarshal(databytes, &benchMessage)
			if err != nil {
				log.Printf("failed to decode bench info json:\n%v\n", err)
			}

			benchStatePayload := &BenchStatePayload{
				Type:  "benchState",
				Left:  benchMessage.Left,
				Right: benchMessage.Right,
			}

			if benchMessage.Left != "" && benchMessage.Right != "" {
				go startFireworkTimer()
			}

			benchInfoBroadcast <- benchStatePayload
		}
	}
}

// syncs actions of a player to the whole server
func handleMoveBroadcast() {
	for {
		player := <-playerStateBroadcast

		for p := range playerList.pList {
			payload := &PlayerStatePayload{Type: "playerState", Id: player.id, State: player.state}
			p.mu.Lock()
			err := p.conn.WriteJSON(*payload)
			p.mu.Unlock()

			if err != nil {
				log.Printf("%v failed to update\n", err)
				p.conn.Close()
				playerList.mu.Lock()
				delete(playerList.pList, p)
				playerList.mu.Unlock()
			}

		}
	}
}

func handleDisconnectBroadcast() {
	for {
		clientInfo := <-clientInfoBroadcast
		for p := range playerList.pList {
			p.mu.Lock()
			err := p.conn.WriteJSON(*clientInfo)
			p.mu.Unlock()
			if err != nil {
				log.Printf("failed to %v %v\n", clientInfo.Type, clientInfo.Id)
				// todo error handle
			}
		}
	}
}

func handleDiningBroadcast() {
	for {
		diningInfo := <-diningInfoBroadcast
		for p := range playerList.pList {
			p.mu.Lock()
			err := p.conn.WriteJSON(*diningInfo)
			p.mu.Unlock()
			if err != nil {
				log.Printf("failed to send dining info to %v\n:%v\n", p.id, err)
				// todo error handle
			}
		}
	}
}

func handleFerrisBroadcast() {
	for {
		ferrisInfo := <-ferrisInfoBroadcast
		for p := range playerList.pList {
			p.mu.Lock()
			err := p.conn.WriteJSON(*ferrisInfo)
			p.mu.Unlock()
			if err != nil {
				log.Printf("failed to send ferris info to %v\n:%v\n", p.id, err)
				// todo error handle
			}
		}
	}
}

func handleFerrisState() {
	lastTime := time.Now()
	reset := true
	maxFrames := 2

	for {
		currentTime := time.Now()
		elapsed := currentTime.Sub(lastTime).Milliseconds()

		if elapsed >= interval {
			if len(ferrisState.players) == 2 {
				if reset {
					ferrisState.mu.Lock()
					ferrisState.frame = 0
					ferrisState.cycle = 0
					reset = false
					ferrisState.mu.Unlock()
				}
			}

			if ferrisState.cycle < 10 {
				ferrisState.mu.Lock()
				ferrisState.cycle++
				ferrisState.mu.Unlock()
			} else {
				ferrisState.mu.Lock()
				ferrisState.cycle = 0
				ferrisState.mu.Unlock()
				if len(ferrisState.players) == 2 {
					maxFrames = 5
				} else {
					reset = true
					maxFrames = 2
				}

				ferrisState.mu.Lock()
				if ferrisState.frame < maxFrames {
					ferrisState.frame++
				} else {
					ferrisState.frame = 0
				}
				ferrisState.mu.Unlock()
			}

			ferrisStatePayload := &FerrisStatePayload{
				Type:    "ferrisState",
				Players: ferrisState.players,
				Frame:   ferrisState.frame,
			}
			ferrisInfoBroadcast <- ferrisStatePayload

			if len(ferrisState.players) >= 2 {
				for p := range playerList.pList {
					for _, ferrisP := range ferrisState.players {
						if ferrisP == p.id {
							p.state.Action = "ferris"
							playerStateBroadcast <- p
							break
						}
					}
				}
			}

			lastTime = currentTime
		}

		time.Sleep(time.Millisecond)
	}
}

func handleBenchBroadcast() {
	var lastBenchState *BenchStatePayload
	for {
		select {
		case benchInfo := <-benchInfoBroadcast:
			if lastBenchState != nil && lastBenchState.ShowFireworks {
				benchInfo.ShowFireworks = true
			}
			lastBenchState = benchInfo
			for p := range playerList.pList {
				p.mu.Lock()
				err := p.conn.WriteJSON(*benchInfo)
				p.mu.Unlock()
				if err != nil {
					log.Printf("failed to send bench info to %v\n:%v\n", p.id, err)
					// todo error handle
				}
			}

		case firework := <-startFirework:
			if lastBenchState.Left != "" && lastBenchState.Right != "" {
				benchStatePayload := &BenchStatePayload{
					Type:          "benchState",
					Left:          lastBenchState.Left,
					Right:         lastBenchState.Right,
					ShowFireworks: firework,
				}
				go func(payload *BenchStatePayload) {
					benchInfoBroadcast <- benchStatePayload
				}(benchStatePayload)
			}
		}
	}
}

func startFireworkTimer() {
	ticker := time.NewTicker(1 * time.Second)

	defer func() {
		ticker.Stop()
		startFirework <- true
	}()

	lastSecond := 0
	for lastSecond < 5 {
		<-ticker.C
		lastSecond++
	}
}
