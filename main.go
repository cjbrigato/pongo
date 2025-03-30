package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"os"

	// "sort" // No longer needed with simplified reconciliation
	"syscall" // Needed for specific network error checking
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

// --- Constants ---
const (
	screenWidth         = 640
	screenHeight        = 480
	paddleWidth         = 15
	paddleHeight        = 80
	ballSize            = 15
	paddleSpeed         = 7.0 // Slightly faster maybe
	initBallSpeed       = 4.5
	defaultPort         = "8081" // Different port from TCP example
	networkTickRate     = 30     // Send state/input this many times per second (adjust based on testing)
	readBufferSize      = 1024
	interpolationBuffer = 0.1              // Seconds of buffer for interpolation (e.g., 100ms)
	clientTimeout       = 10 * time.Second // Time before host considers client disconnected
	packetHeaderSize    = 1                // Simple header: 1 byte for packet type
)

// Packet Types (Header Byte)
const (
	PacketTypeClientInput byte = 1
	PacketTypeGameState   byte = 2
	PacketTypeClientHello byte = 3 // Initial connection packet
	PacketTypeServerHello byte = 4 // Server response to Hello
)

// --- Data Structures for Networking (Binary) ---

// NetGameState represents the state sent from host to client
type NetGameState struct {
	// Header (Not explicitly part of struct, handled during read/write)
	Sequence     uint32  // Server state sequence number
	AckClientSeq uint32  // Last client input sequence number processed by server
	Timestamp    float64 // Server time when this state was generated

	Player1Y     float32
	Player2Y     float32 // Authoritative position of client paddle
	BallX        float32
	BallY        float32
	BallVX       float32 // Include velocity for potential advanced interpolation/prediction
	BallVY       float32
	Player1Score uint8
	Player2Score uint8
	GameStarted  bool // Use a single byte (0 or 1) for bool
}

// NetClientInput represents the input sent from client to host
type NetClientInput struct {
	// Header (Not explicitly part of struct, handled during read/write)
	Sequence uint32  // Client input sequence number
	PlayerY  float32 // The Y position the client *intends* its paddle to be at
	// Could add input flags (UpPressed, DownPressed) instead/as well
}

// Helper for client prediction/reconciliation
type ClientInputRecord struct {
	Seq uint32
	Y   float32
}

// --- Global Images ---
var (
	paddleImage *ebiten.Image
	ballImage   *ebiten.Image
	bgColor     = color.RGBA{0x00, 0x00, 0x00, 0xff} // Black
	fgColor     = color.RGBA{0xff, 0xff, 0xff, 0xff} // White
)

// --- Game Struct ---
type Game struct {
	isHost bool
	conn   net.PacketConn // UDP connection
	// We need the remote address to send UDP packets back
	// Host stores clientAddr, Client stores serverAddr
	remoteAddr net.Addr

	// Network sequence numbers
	clientInputSeq uint32 // Client: Last input sequence sent
	serverStateSeq uint32 // Host: Last state sequence sent
	lastAckedSeq   uint32 // Client: Last input sequence server acknowledged
	lastRecvSeq    uint32 // Sequence number of the last valid packet received (server uses for input, client for state)

	// Game state variables
	player1Y     float64 // Authoritative on Host, interpolated on Client
	player2Y     float64 // Authoritative on Host, PREDICTED on Client, interpolated view of opponent on Host (less critical)
	ballX        float64 // Authoritative on Host, interpolated on Client
	ballY        float64
	ballVX       float64 // Only used by Host for physics
	ballVY       float64
	player1Score int
	player2Score int

	// State Management
	gameStarted       bool // Has the opponent connected and acknowledged?
	connectionReady   bool // Has initial handshake completed?
	lastHeardFromPeer time.Time
	networkError      error

	// Timing
	lastNetworkSend time.Time
	tickDuration    time.Duration

	// Client-Side Prediction / Interpolation
	isPlayer2             bool                // Convenience flag for client
	pendingInputs         []ClientInputRecord // Client: Inputs sent but not yet acked by server
	interpolationTarget   NetGameState        // Client: The latest state received from server
	interpolationPrevious NetGameState        // Client: The state before the latest one
	interpolationStart    time.Time           // Client: Time when the current interpolation period started
	displayBallX          float64             // Client: Interpolated ball X
	displayBallY          float64             // Client: Interpolated ball Y
	displayPlayer1Y       float64             // Client: Interpolated opponent Y
	lastStateTimestamp    float64             // Client: Timestamp of the latest state received

	// Buffer for reading packets
	readBuf []byte
}

// NewGame initializes common elements
func NewGame(host bool) *Game {
	g := &Game{
		isHost:       host,
		player1Y:     screenHeight/2 - paddleHeight/2,
		player2Y:     screenHeight/2 - paddleHeight/2,
		ballX:        screenWidth/2 - ballSize/2,
		ballY:        screenHeight/2 - ballSize/2,
		tickDuration: time.Second / time.Duration(networkTickRate),
		readBuf:      make([]byte, readBufferSize),
		isPlayer2:    !host, // Client controls player 2
	}
	// Initial display positions match logical positions
	g.displayBallX = g.ballX
	g.displayBallY = g.ballY
	g.displayPlayer1Y = g.player1Y
	return g
}

// --- Binary Serialization Helpers ---

// --- CORRECTED writePacket ---
func writePacket(w io.Writer, packetType byte, data interface{}) error {
	// Write header byte
	if err := binary.Write(w, binary.LittleEndian, packetType); err != nil {
		return fmt.Errorf("write header failed: %w", err)
	}

	// Only write the data payload if it's actually provided (not nil)
	if data != nil {
		// Write struct data
		if err := binary.Write(w, binary.LittleEndian, data); err != nil {
			return fmt.Errorf("write data payload failed (type: %T): %w", data, err)
		}
	}
	return nil
}

// --- END CORRECTION ---

func readGameState(r io.Reader) (NetGameState, error) {
	var state NetGameState
	// Read GameStarted bool as uint8
	var gameStartedUint8 uint8
	err := binary.Read(r, binary.LittleEndian, &state.Sequence)
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.AckClientSeq)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.Timestamp)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.Player1Y)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.Player2Y)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.BallX)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.BallY)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.BallVX)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.BallVY)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.Player1Score)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &state.Player2Score)
	}
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &gameStartedUint8)
	}

	if err != nil {
		// Distinguish EOF from other errors if necessary, though UDP shouldn't give EOF easily
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return NetGameState{}, fmt.Errorf("read GameState failed (incomplete data): %w", err)
		}
		return NetGameState{}, fmt.Errorf("read GameState failed: %w", err)
	}
	state.GameStarted = (gameStartedUint8 != 0)
	return state, nil
}

func readClientInput(r io.Reader) (NetClientInput, error) {
	var input NetClientInput
	err := binary.Read(r, binary.LittleEndian, &input.Sequence)
	if err == nil {
		err = binary.Read(r, binary.LittleEndian, &input.PlayerY)
	}

	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return NetClientInput{}, fmt.Errorf("read ClientInput failed (incomplete data): %w", err)
		}
		return NetClientInput{}, fmt.Errorf("read ClientInput failed: %w", err)
	}
	return input, nil
}

// --- Host Logic ---

func (g *Game) updateHost() error {
	now := time.Now()

	// 1. Read Incoming Packets (Non-blocking read attempt)
	for {
		g.conn.SetReadDeadline(now.Add(1 * time.Millisecond)) // Very short deadline
		n, addr, err := g.conn.ReadFrom(g.readBuf)
		g.conn.SetReadDeadline(time.Time{}) // Clear deadline immediately

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break // No more packets waiting right now
			}
			// Ignore closed connection errors during shutdown maybe
			if errors.Is(err, net.ErrClosed) {
				break
			}
			// EOF typically means connection closed for connection-oriented protocols,
			// but for UDP it might just be an error. Log it.
			if !errors.Is(err, io.EOF) {
				log.Printf("Host read error: %v", err)
				// Decide if error is fatal. For now, continue maybe?
				// Or return error to stop game? Let's return it.
				g.networkError = fmt.Errorf("host read error: %w", err)
				return g.networkError
			}
			break // Stop reading on EOF or other errors
		}

		// If we don't have a client address yet, store it. Ignore packets from others for now.
		// We only really accept the first responder for this simple 1v1 game.
		if g.remoteAddr == nil {
			// Process potential ClientHello first
			readerHello := bytes.NewReader(g.readBuf[:n])
			var packetTypeHello byte
			if err := binary.Read(readerHello, binary.LittleEndian, &packetTypeHello); err == nil {
				if packetTypeHello == PacketTypeClientHello {
					log.Printf("Host: Received ClientHello from %s", addr)
					g.remoteAddr = addr // Store client address
					g.connectionReady = true
					g.lastHeardFromPeer = now
					// Send ServerHello back immediately
					buf := &bytes.Buffer{}
					// Passing nil data here, relies on the fix in writePacket
					if err := writePacket(buf, PacketTypeServerHello, nil); err == nil {
						g.conn.WriteTo(buf.Bytes(), g.remoteAddr)
						log.Printf("Host: Sent ServerHello to %s", g.remoteAddr)
					} else {
						log.Printf("Host: Error writing ServerHello: %v", err)
					}
					continue // Handled this packet
				} else {
					log.Printf("Host: Ignored packet type %d from unknown address %s", packetTypeHello, addr)
					continue
				}
			} else {
				log.Printf("Host: Error reading packet type from unknown address %s: %v", addr, err)
				continue
			}
		} else if g.remoteAddr.String() != addr.String() {
			log.Printf("Host: Ignored packet from unexpected address %s (expected %s)", addr, g.remoteAddr)
			continue // Ignore packets from other addresses
		}

		// Process the received packet from the known client
		g.lastHeardFromPeer = now
		reader := bytes.NewReader(g.readBuf[:n])
		var packetTypeByte byte
		if err := binary.Read(reader, binary.LittleEndian, &packetTypeByte); err != nil {
			log.Printf("Host: Error reading packet type from %s: %v", g.remoteAddr, err)
			continue
		}

		switch packetTypeByte {
		case PacketTypeClientInput:
			input, err := readClientInput(reader)
			if err != nil {
				log.Printf("Host: Error decoding client input from %s: %v", g.remoteAddr, err)
				continue
			}
			// Process input only if it's newer than the last processed one
			// Handle sequence number wrap around? Unlikely for short game, but possible.
			// Simple check: if new seq is much smaller than last, assume wrap around.
			// Or just use > which handles most cases until wrap.
			if input.Sequence > g.lastRecvSeq { // || (g.lastRecvSeq > (math.MaxUint32 - 1000) && input.Sequence < 1000) { // Basic wrap check
				g.lastRecvSeq = input.Sequence
				// Update Player 2's position based on client input (Authoritative)
				g.player2Y = float64(input.PlayerY)
				// Clamp server-side
				g.player2Y = math.Max(0, math.Min(screenHeight-paddleHeight, g.player2Y))
			} else {
				// log.Printf("Host: Discarded old/duplicate input packet %d (last was %d)", input.Sequence, g.lastRecvSeq)
			}

		case PacketTypeClientHello:
			// Client might resend Hello if ServerHello was lost. Respond again.
			log.Printf("Host: Received redundant ClientHello from %s", addr)
			// Send ServerHello back immediately
			buf := &bytes.Buffer{}
			// Passing nil data here, relies on the fix in writePacket
			if err := writePacket(buf, PacketTypeServerHello, nil); err == nil {
				g.conn.WriteTo(buf.Bytes(), g.remoteAddr)
				log.Printf("Host: Re-Sent ServerHello to %s", g.remoteAddr)
			} else {
				log.Printf("Host: Error writing ServerHello (redundant): %v", err)
			}

		default:
			log.Printf("Host: Received unknown packet type %d from %s", packetTypeByte, g.remoteAddr)
		}
	}

	// Check for client timeout only if connection was established
	if g.connectionReady && now.Sub(g.lastHeardFromPeer) > clientTimeout {
		g.networkError = fmt.Errorf("client timed out")
		log.Println("Host: Client timed out.")
		return g.networkError
	}

	// Don't run game logic until client is connected
	if !g.connectionReady {
		return nil
	}

	// Only start game physics once client is ready and game hasn't started yet
	if !g.gameStarted {
		g.gameStarted = true
		g.resetBall(true) // Start the ball once connected
		log.Println("Host: Game started!")
	}

	// 2. Process Host Input (Player 1: W/S)
	if ebiten.IsKeyPressed(ebiten.KeyW) {
		g.player1Y -= paddleSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyS) {
		g.player1Y += paddleSpeed
	}
	g.player1Y = math.Max(0, math.Min(screenHeight-paddleHeight, g.player1Y))

	// 3. Run Authoritative Physics Simulation (if game started)
	g.runHostPhysics() // Ball movement, collisions, scoring

	// 4. Send Game State Periodically
	if now.Sub(g.lastNetworkSend) >= g.tickDuration {
		g.lastNetworkSend = now
		g.serverStateSeq++ // Increment state sequence number

		// Convert gameStarted bool to uint8 for binary writing
		var gameStartedUint8 uint8 = 0
		if g.gameStarted {
			gameStartedUint8 = 1
		}

		state := NetGameState{
			Sequence:     g.serverStateSeq,
			AckClientSeq: g.lastRecvSeq,                 // Tell client which input we last processed
			Timestamp:    float64(now.UnixNano()) / 1e9, // Use seconds with fractions
			Player1Y:     float32(g.player1Y),
			Player2Y:     float32(g.player2Y),
			BallX:        float32(g.ballX),
			BallY:        float32(g.ballY),
			BallVX:       float32(g.ballVX),
			BallVY:       float32(g.ballVY),
			Player1Score: uint8(g.player1Score),
			Player2Score: uint8(g.player2Score),
			GameStarted:  g.gameStarted, // Keep bool in struct for clarity
		}

		buf := &bytes.Buffer{}
		// Manually write the bool as uint8 at the end
		err := writePacket(buf, PacketTypeGameState, &NetGameState{ // Pass pointer for direct write
			Sequence:     state.Sequence,
			AckClientSeq: state.AckClientSeq,
			Timestamp:    state.Timestamp,
			Player1Y:     state.Player1Y,
			Player2Y:     state.Player2Y,
			BallX:        state.BallX,
			BallY:        state.BallY,
			BallVX:       state.BallVX,
			BallVY:       state.BallVY,
			Player1Score: state.Player1Score,
			Player2Score: state.Player2Score,
			// Can't directly include bool here for binary.Write on struct
		})
		// Append the bool manually as uint8 AFTER the struct write
		if err == nil {
			err = binary.Write(buf, binary.LittleEndian, gameStartedUint8)
		}

		if err != nil {
			log.Printf("Host: Error encoding game state: %v", err)
			// Decide if fatal, maybe continue
		} else {
			_, err := g.conn.WriteTo(buf.Bytes(), g.remoteAddr)
			if err != nil {
				// Log error, but UDP WriteTo might fail for various non-fatal reasons
				// (e.g., network unreachable temporarily). Don't necessarily disconnect.
				if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENETUNREACH) && !errors.Is(err, net.ErrClosed) { // Example errors to potentially ignore
					log.Printf("Host: Error sending game state: %v", err)
				}
				// Only treat persistent errors or specific types as fatal if needed.
			}
		}
	}

	return nil
}

// runHostPhysics - Unchanged from UDP version
func (g *Game) runHostPhysics() {
	if !g.gameStarted {
		return
	}
	// Skipping score timer logic for brevity, assume immediate restart

	// Move ball
	g.ballX += g.ballVX
	g.ballY += g.ballVY

	// Top/Bottom Wall Collision
	if g.ballY <= 0 && g.ballVY < 0 {
		g.ballY = 0
		g.ballVY = -g.ballVY
	}
	if g.ballY+ballSize >= screenHeight && g.ballVY > 0 {
		g.ballY = screenHeight - ballSize
		g.ballVY = -g.ballVY
	}

	// Paddle Collision Check
	p1Left, p1Right, p1Top, p1Bottom := 0.0, float64(paddleWidth), g.player1Y, g.player1Y+paddleHeight
	p2Left, p2Right, p2Top, p2Bottom := float64(screenWidth-paddleWidth), float64(screenWidth), g.player2Y, g.player2Y+paddleHeight
	ballLeft, ballRight, ballTop, ballBottom := g.ballX, g.ballX+ballSize, g.ballY, g.ballY+ballSize

	// Player 1 (Host) Collision
	if g.ballVX < 0 && ballLeft <= p1Right && ballRight > p1Left && ballBottom > p1Top && ballTop < p1Bottom {
		g.ballVX = math.Abs(g.ballVX) // Ensure positive velocity
		g.ballX = p1Right
		// Add bounce physics if desired (change angle based on hit location)
		centerY := p1Top + paddleHeight/2
		hitY := ballTop + ballSize/2
		ratio := (hitY - centerY) / (paddleHeight / 2)                 // -1 to 1
		angle := ratio * (math.Pi / 4)                                 // Max bounce angle (e.g., 45 degrees)
		speed := math.Sqrt(g.ballVX*g.ballVX+g.ballVY*g.ballVY) * 1.05 // Increase speed slightly
		g.ballVX = speed * math.Cos(angle)
		g.ballVY = speed * math.Sin(angle)

	}

	// Player 2 (Client) Collision
	if g.ballVX > 0 && ballRight >= p2Left && ballLeft < p2Right && ballBottom > p2Top && ballTop < p2Bottom {
		g.ballVX = -math.Abs(g.ballVX) // Ensure negative velocity
		g.ballX = p2Left - ballSize
		// Add bounce physics
		centerY := p2Top + paddleHeight/2
		hitY := ballTop + ballSize/2
		ratio := (hitY - centerY) / (paddleHeight / 2)                 // -1 to 1
		angle := ratio * (math.Pi / 4)                                 // Max bounce angle
		speed := math.Sqrt(g.ballVX*g.ballVX+g.ballVY*g.ballVY) * 1.05 // Increase speed slightly
		g.ballVX = -speed * math.Cos(angle)                            // Negative X direction
		g.ballVY = speed * math.Sin(angle)
	}

	// Scoring
	scored := false
	serverIsP1 := true
	if ballRight < 0 {
		g.player2Score++
		scored = true
		serverIsP1 = true
	}
	if ballLeft > screenWidth {
		g.player1Score++
		scored = true
		serverIsP1 = false
	}
	if scored {
		g.resetBall(serverIsP1)
	}
}

// resetBall (Only called by Host)
func (g *Game) resetBall(serverPlayer1 bool) {
	g.ballX = screenWidth/2 - ballSize/2
	g.ballY = screenHeight/2 - ballSize/2
	speed := initBallSpeed
	angle := (rand.Float64()*math.Pi/2 - math.Pi/4) // -45 to +45 degrees
	if !serverPlayer1 {
		angle += math.Pi
	}
	g.ballVX = speed * math.Cos(angle)
	g.ballVY = speed * math.Sin(angle)
	// No score timer needed for basic UDP version, relies on state updates
}

// --- Client Logic ---

func (g *Game) updateClient() error {
	now := time.Now()
	nowTimestamp := float64(now.UnixNano()) / 1e9

	// 1. Read Incoming Packets
	for {
		g.conn.SetReadDeadline(now.Add(1 * time.Millisecond))
		n, addr, err := g.conn.ReadFrom(g.readBuf)
		g.conn.SetReadDeadline(time.Time{})

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break // No more packets
			}
			if errors.Is(err, net.ErrClosed) {
				break // Ignore errors after connection closed
			}
			if !errors.Is(err, io.EOF) {
				log.Printf("Client read error: %v", err)
				g.networkError = fmt.Errorf("client read error: %w", err)
				return g.networkError
			}
			break
		}

		// Ensure packet is from the server address we connected to (or expect)
		if g.remoteAddr == nil {
			// This shouldn't happen if handshake worked, but handle defensively
			log.Printf("Client: Received packet before server address known from %s", addr)
			continue
		}
		if g.remoteAddr.String() != addr.String() {
			log.Printf("Client: Ignored packet from unexpected address %s (expected %s)", addr, g.remoteAddr)
			continue
		}

		// Process packet
		g.lastHeardFromPeer = now
		reader := bytes.NewReader(g.readBuf[:n])
		var packetTypeByte byte
		if err := binary.Read(reader, binary.LittleEndian, &packetTypeByte); err != nil {
			log.Printf("Client: Error reading packet type: %v", err)
			continue
		}

		switch packetTypeByte {
		case PacketTypeGameState:
			newState, err := readGameState(reader)
			if err != nil {
				log.Printf("Client: Error decoding game state: %v", err)
				continue
			}

			// Discard old or out-of-order state packets
			// Handle sequence number wrap around?
			if newState.Sequence <= g.lastRecvSeq { // && !(g.lastRecvSeq > (math.MaxUint32 - 1000) && newState.Sequence < 1000) {
				// log.Printf("Client: Discarded old state packet %d (last was %d)", newState.Sequence, g.lastRecvSeq)
				continue
			}
			g.lastRecvSeq = newState.Sequence

			// Store previous state for interpolation
			g.interpolationPrevious = g.interpolationTarget
			g.interpolationTarget = newState
			g.interpolationStart = now // Start interpolating from now
			g.lastStateTimestamp = newState.Timestamp

			// Update authoritative state for non-interpolated/non-predicted things
			g.player1Score = int(newState.Player1Score)
			g.player2Score = int(newState.Player2Score)
			// Sync game status (reading the GameStarted bool correctly is handled in readGameState)
			g.gameStarted = newState.GameStarted
			if g.gameStarted && !g.connectionReady {
				// This should not happen if handshake works, but defensively...
				g.connectionReady = true // Mark connected if game started message arrives
				log.Println("Client: Game started signal received, marking connection ready.")
			}

			// --- Reconciliation for Player 2's Paddle ---
			// Server tells us the last input it processed (AckClientSeq)
			if newState.AckClientSeq > g.lastAckedSeq { // || (g.lastAckedSeq > (math.MaxUint32 - 1000) && newState.AckClientSeq < 1000) {
				g.lastAckedSeq = newState.AckClientSeq

				// Remove acknowledged inputs from the pending buffer
				i := 0
				for _, input := range g.pendingInputs {
					// Keep inputs that are *newer* than the one the server just ack'd
					if input.Seq > g.lastAckedSeq {
						g.pendingInputs[i] = input
						i++
					}
				}
				g.pendingInputs = g.pendingInputs[:i]

				// Start reconciliation from the server's authoritative position for Player 2
				reconciledP2Y := float64(newState.Player2Y)

				// Re-apply *remaining* pending inputs ON TOP of the server's state
				// This replays inputs the server hasn't processed yet.
				for _, record := range g.pendingInputs {
					// In this simple model, the input IS the target Y.
					// We just apply the effect of the input again.
					// More complex: Simulate fixed steps based on input delta & time.
					// Simplest: Just snap to the latest *unacknowledged* target Y.
					// Let's stick to snapping to the latest unacked input's target Y
					reconciledP2Y = float64(record.Y)
				}
				// Clamp the reconciled position
				reconciledP2Y = math.Max(0, math.Min(screenHeight-paddleHeight, reconciledP2Y))

				// Update the local predicted position
				g.player2Y = reconciledP2Y
			}

		case PacketTypeServerHello:
			if !g.connectionReady {
				log.Printf("Client: Received ServerHello from %s. Connection ready!", addr)
				g.connectionReady = true // Handshake complete
			} // Ignore redundant Hellos
			g.lastHeardFromPeer = now

		default:
			log.Printf("Client: Received unknown packet type %d", packetTypeByte)
		}
	}

	// Check for server timeout
	// Only check timeout *after* handshake is complete
	if g.connectionReady && now.Sub(g.lastHeardFromPeer) > clientTimeout {
		g.networkError = fmt.Errorf("server timed out")
		log.Println("Client: Server timed out.")
		return g.networkError
	}

	// Wait for connection handshake (triggered by ServerHello)
	if !g.connectionReady {
		// Periodically send ClientHello until we get ServerHello
		if now.Sub(g.lastNetworkSend) >= 500*time.Millisecond { // Send hello every 500ms
			g.lastNetworkSend = now
			log.Println("Client: Sending ClientHello...")
			buf := &bytes.Buffer{}
			// Passing nil data relies on fix in writePacket
			if err := writePacket(buf, PacketTypeClientHello, nil); err == nil {
				if _, err := g.conn.WriteTo(buf.Bytes(), g.remoteAddr); err != nil {
					// Ignore common errors like connection refused if server not up yet
					if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, net.ErrClosed) {
						log.Printf("Client: Error sending ClientHello: %v", err)
					}
				}
			} else {
				log.Printf("Client: Error encoding ClientHello: %v", err)
			}
		}
		return nil // Don't proceed until connected
	}

	// Don't run game input/prediction if host hasn't signaled start via GameState
	if !g.gameStarted {
		// Still need to process incoming packets (done above) to get the start signal.
		// Maybe send periodic keep-alives (empty input packets?) if needed, but probably okay to wait.
		return nil
	}

	// 2. Process Local Input (Player 2: Up/Down) & Client-Side Prediction
	inputY := g.player2Y // Start with current predicted position
	moved := false
	if ebiten.IsKeyPressed(ebiten.KeyArrowUp) {
		inputY -= paddleSpeed
		moved = true
	}
	if ebiten.IsKeyPressed(ebiten.KeyArrowDown) {
		inputY += paddleSpeed
		moved = true
	}
	// Clamp predicted position immediately for responsiveness
	inputY = math.Max(0, math.Min(screenHeight-paddleHeight, inputY))

	// Apply prediction: Update local g.player2Y *immediately* if moved
	if moved {
		g.player2Y = inputY
	}

	// 3. Send Input Periodically or If Moved
	// Only send input if connected and game has started
	if g.connectionReady && g.gameStarted && (moved || now.Sub(g.lastNetworkSend) >= g.tickDuration) {
		g.lastNetworkSend = now
		g.clientInputSeq++

		// Record the input for reconciliation
		record := ClientInputRecord{Seq: g.clientInputSeq, Y: float32(g.player2Y)}
		g.pendingInputs = append(g.pendingInputs, record)
		// Limit size of pending inputs buffer?
		// if len(g.pendingInputs) > 60 { // Example limit
		//     g.pendingInputs = g.pendingInputs[1:] // Remove oldest
		// }

		// Prepare input packet
		input := NetClientInput{
			Sequence: g.clientInputSeq,
			PlayerY:  float32(g.player2Y), // Send the predicted position
		}
		buf := &bytes.Buffer{}
		err := writePacket(buf, PacketTypeClientInput, input) // Pass input struct directly
		if err != nil {
			log.Printf("Client: Error encoding input: %v", err)
		} else {
			_, err := g.conn.WriteTo(buf.Bytes(), g.remoteAddr)
			if err != nil {
				if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENETUNREACH) && !errors.Is(err, net.ErrClosed) {
					log.Printf("Client: Error sending input: %v", err)
				}
			}
		}
	}

	// 4. Perform Interpolation for Ball and Opponent Paddle
	// Only interpolate if game has started
	if g.gameStarted {
		g.interpolateState(nowTimestamp)
	}

	return nil
}

// interpolateState calculates display positions based on buffered states
func (g *Game) interpolateState(nowTimestamp float64) {
	if g.interpolationTarget.Sequence == 0 { // No state received yet
		// Keep display at initial/last known logical position before first state
		g.displayBallX = g.ballX
		g.displayBallY = g.ballY
		g.displayPlayer1Y = g.player1Y
		return
	}

	// Target rendering time in the past to smooth over network jitter
	renderTimestamp := nowTimestamp - interpolationBuffer

	// Check if the target render time falls between the previous and current state timestamps
	if renderTimestamp <= g.interpolationPrevious.Timestamp || g.interpolationPrevious.Sequence == 0 || g.interpolationTarget.Timestamp <= g.interpolationPrevious.Timestamp {
		// Not enough history, render time is too far back, or timestamps are invalid/equal: snap to the latest known state (interpolationTarget)
		g.displayBallX = float64(g.interpolationTarget.BallX)
		g.displayBallY = float64(g.interpolationTarget.BallY)
		g.displayPlayer1Y = float64(g.interpolationTarget.Player1Y)
	} else if renderTimestamp >= g.interpolationTarget.Timestamp {
		// Render time is ahead of the latest known server state.
		// Extrapolation: Predict future position based on latest state's velocity.
		// Be careful: Can lead to overshoot or weirdness if velocity changes rapidly.
		timeDelta := renderTimestamp - g.interpolationTarget.Timestamp
		extrapolatedBallX := float64(g.interpolationTarget.BallX) + float64(g.interpolationTarget.BallVX)*timeDelta
		extrapolatedBallY := float64(g.interpolationTarget.BallY) + float64(g.interpolationTarget.BallVY)*timeDelta
		// We don't have opponent paddle velocity, so just clamp it to the last known position.
		extrapolatedPlayer1Y := float64(g.interpolationTarget.Player1Y)

		// Clamp extrapolation to screen bounds (basic sanity check)
		g.displayBallX = math.Max(0, math.Min(screenWidth-ballSize, extrapolatedBallX))
		g.displayBallY = math.Max(0, math.Min(screenHeight-ballSize, extrapolatedBallY))
		g.displayPlayer1Y = extrapolatedPlayer1Y // Clamping already handled server-side mostly

	} else {
		// Interpolate between previous and target states
		timeDiff := g.interpolationTarget.Timestamp - g.interpolationPrevious.Timestamp
		// Avoid division by zero, although the checks above should prevent timeDiff <= 0
		if timeDiff <= 1e-9 { // Use a small epsilon instead of == 0 for float comparison
			g.displayBallX = float64(g.interpolationTarget.BallX)
			g.displayBallY = float64(g.interpolationTarget.BallY)
			g.displayPlayer1Y = float64(g.interpolationTarget.Player1Y)
			return
		}

		factor := (renderTimestamp - g.interpolationPrevious.Timestamp) / timeDiff
		factor = math.Max(0.0, math.Min(1.0, factor)) // Clamp factor [0, 1]

		g.displayBallX = lerp(float64(g.interpolationPrevious.BallX), float64(g.interpolationTarget.BallX), factor)
		g.displayBallY = lerp(float64(g.interpolationPrevious.BallY), float64(g.interpolationTarget.BallY), factor)
		g.displayPlayer1Y = lerp(float64(g.interpolationPrevious.Player1Y), float64(g.interpolationTarget.Player1Y), factor)
	}
}

// Linear interpolation helper
func lerp(a, b, t float64) float64 {
	return a + (b-a)*t
}

// --- Common Ebiten Methods ---

func (g *Game) Update() error {
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		if g.conn != nil {
			g.conn.Close() // Close the socket
		}
		return ebiten.Termination
	}

	// Check for critical network error stored previously
	if g.networkError != nil {
		if g.conn != nil {
			g.conn.Close()
			g.conn = nil
		}
		// Return the error to terminate Ebiten loop
		log.Printf("Terminating due to network error: %v", g.networkError)
		return g.networkError
	}

	// Ensure connection exists before updating (should always be true after setup unless error occurred)
	if g.conn == nil {
		// If no network error stored, this is unexpected.
		if g.networkError == nil {
			errMsg := "update called without active connection or network error"
			log.Println(errMsg)
			return fmt.Errorf(errMsg)
		}
		// Otherwise, the error was handled above.
		return nil // Don't try to update more
	}

	var err error
	if g.isHost {
		err = g.updateHost()
	} else {
		err = g.updateClient()
	}

	// If update returned an error, store it and return it to terminate
	if err != nil {
		g.networkError = err // Store the error
		if g.conn != nil {   // Attempt to close conn if error occurred
			g.conn.Close()
			g.conn = nil
		}
		return err // Terminate Ebiten loop
	}

	return nil // Continue game loop
}

func (g *Game) Draw(screen *ebiten.Image) {
	screen.Fill(bgColor)

	// Display network error prominently if it occurred
	if g.networkError != nil {
		msg := fmt.Sprintf("Network Error: %v", g.networkError)
		width := len(msg) * 7 // Approx text width
		ebitenutil.DebugPrintAt(screen, msg, screenWidth/2-width/2, screenHeight/2-10)
		return
	}

	// Display connection status before handshake complete
	if !g.connectionReady {
		msg := "Connecting..."
		if g.isHost {
			msg = "Waiting for connection..."
		}
		width := len(msg) * 7
		ebitenutil.DebugPrintAt(screen, msg, screenWidth/2-width/2, screenHeight/2-10)
		return
	}

	// Display waiting message if connected but game hasn't started
	if !g.gameStarted && g.connectionReady {
		msg := "Waiting for game to start..."
		width := len(msg) * 7
		ebitenutil.DebugPrintAt(screen, msg, screenWidth/2-width/2, screenHeight/2-10)
		// Draw paddles in initial state while waiting? Optional.
		// op1 := &ebiten.DrawImageOptions{}
		// op1.GeoM.Translate(0, screenHeight/2 - paddleHeight/2)
		// screen.DrawImage(paddleImage, op1)
		// op2 := &ebiten.DrawImageOptions{}
		// op2.GeoM.Translate(screenWidth-paddleWidth, screenHeight/2 - paddleHeight/2)
		// screen.DrawImage(paddleImage, op2)
		return
	}

	// --- Draw Game Elements (Only if gameStarted is true) ---
	if g.gameStarted {
		// Player 1 Paddle
		p1DrawY := g.player1Y // Host draws authoritative P1
		if !g.isHost {
			p1DrawY = g.displayPlayer1Y // Client draws interpolated P1
		}
		op1 := &ebiten.DrawImageOptions{}
		op1.GeoM.Translate(0, p1DrawY)
		screen.DrawImage(paddleImage, op1)

		// Player 2 Paddle
		p2DrawY := g.player2Y // Host draws authoritative P2, Client draws PREDICTED P2
		op2 := &ebiten.DrawImageOptions{}
		op2.GeoM.Translate(screenWidth-paddleWidth, p2DrawY)
		screen.DrawImage(paddleImage, op2)

		// Ball
		ballDrawX := g.ballX // Host draws authoritative ball
		ballDrawY := g.ballY
		if !g.isHost {
			ballDrawX = g.displayBallX // Client draws interpolated ball
			ballDrawY = g.displayBallY
		}
		opBall := &ebiten.DrawImageOptions{}
		opBall.GeoM.Translate(ballDrawX, ballDrawY)
		screen.DrawImage(ballImage, opBall)

		// Scores
		scoreStr := fmt.Sprintf("%d   %d", g.player1Score, g.player2Score)
		textWidth := len(scoreStr) * 7
		ebitenutil.DebugPrintAt(screen, scoreStr, screenWidth/2-textWidth/2, 10)

		// Controls Hint
		controls := "P1 (Host): W/S | P2 (Client): Up/Down | ESC: Quit"
		ebitenutil.DebugPrintAt(screen, controls, 5, screenHeight-15)

		// Optional: Debug info
		// if !g.isHost {
		//     debugStr := fmt.Sprintf("SeqOut:%d Ack:%d Pend:%d", g.clientInputSeq, g.lastAckedSeq, len(g.pendingInputs))
		//     ebitenutil.DebugPrintAt(screen, debugStr, 5, 5)
		//     debugStr2 := fmt.Sprintf("StateIn S:%d T:%.2f", g.interpolationTarget.Sequence, g.lastStateTimestamp)
		//      ebitenutil.DebugPrintAt(screen, debugStr2, 5, 20)
		// } else {
		//      debugStr := fmt.Sprintf("StateOut S:%d LastIn:%d", g.serverStateSeq, g.lastRecvSeq)
		//      ebitenutil.DebugPrintAt(screen, debugStr, 5, 5)
		// }
	}
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return screenWidth, screenHeight
}

// --- Network Setup ---

func setupUDPConn(address string) (net.PacketConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP address '%s' failed: %w", address, err)
	}
	conn, err := net.ListenPacket("udp", udpAddr.String())
	if err != nil {
		return nil, fmt.Errorf("listen packet on '%s' failed: %w", udpAddr.String(), err)
	}
	log.Printf("UDP socket listening/sending on %s", conn.LocalAddr())
	return conn, nil
}

// --- Main Function ---
func main() {
	hostFlag := flag.Bool("host", false, "Run as the host")
	connectFlag := flag.String("connect", "", "Connect to host address (e.g., localhost:8081 or IP:PORT)")
	portFlag := flag.String("port", defaultPort, "Port for the host to listen on, or client to send from (optional for client)")
	flag.Parse()

	isHost := *hostFlag
	connectAddrStr := *connectFlag

	if isHost && connectAddrStr != "" {
		fmt.Println("Error: Cannot specify both -host and -connect")
		os.Exit(1)
	}
	if !isHost && connectAddrStr == "" {
		fmt.Println("Usage:")
		fmt.Println("  Host:   go run main.go -host [-port <port>]")
		fmt.Println("  Client: go run main.go -connect <host_address:port> [-port <local_port>]")
		os.Exit(1)
	}

	rand.Seed(time.Now().UnixNano())

	// Create images
	paddleImage = ebiten.NewImage(paddleWidth, paddleHeight)
	paddleImage.Fill(fgColor)
	ballImage = ebiten.NewImage(ballSize, ballSize)
	ballImage.Fill(fgColor)

	var game *Game
	var listenAddr string
	var remoteAddr *net.UDPAddr // Client needs host address parsed

	if isHost {
		listenAddr = ":" + *portFlag // Host listens on specified port, all interfaces
		game = NewGame(true)
	} else {
		// Client needs to resolve the host address
		var err error
		remoteAddr, err = net.ResolveUDPAddr("udp", connectAddrStr)
		if err != nil {
			log.Fatalf("Failed to resolve host address %s: %v", connectAddrStr, err)
		}
		// Client should usually listen on ":0" to let OS pick a free port.
		// Listening on the default port might conflict if host is on same machine.
		listenAddr = ":" + *portFlag
		if *portFlag == defaultPort || *portFlag == "" { // Let OS choose if default or empty specified
			listenAddr = ":0"
			log.Printf("Client local port not specified or default, using OS-assigned port (%s)", listenAddr)
		} else {
			log.Printf("Client attempting to use specified local port: %s", listenAddr)
		}
		game = NewGame(false)
		game.remoteAddr = remoteAddr // Client knows where to send initially
	}

	// Setup UDP connection
	conn, err := setupUDPConn(listenAddr)
	if err != nil {
		log.Fatalf("Failed to set up UDP connection on %s: %v", listenAddr, err)
	}
	// Defer close *after* assigning to game struct
	game.conn = conn
	defer func() {
		if game.conn != nil {
			log.Println("Closing UDP connection...")
			game.conn.Close()
		}
	}()
	game.lastHeardFromPeer = time.Now() // Initialize timer

	// Ebiten setup
	ebiten.SetWindowSize(screenWidth, screenHeight)
	title := "Go Pong UDP - Client"
	if isHost {
		title = "Go Pong UDP - Host"
	}
	ebiten.SetWindowTitle(title)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeDisabled)
	ebiten.SetTPS(60) // Run Ebiten's Update loop at 60 TPS

	log.Println("Starting game loop...")
	if runErr := ebiten.RunGame(game); runErr != nil {
		// Check if the error is the normal termination signal
		if errors.Is(runErr, ebiten.Termination) {
			log.Println("Game exited normally (ESC pressed or termination signal).")
		} else if game.networkError != nil && errors.Is(runErr, game.networkError) {
			// If the error from RunGame is the same as our stored network error, log it cleanly
			log.Printf("Game exited due to network error: %v", game.networkError)
		} else {
			// Otherwise, log the unexpected error from RunGame
			log.Fatalf("Ebiten run failed with unexpected error: %v", runErr)
		}
	} else {
		log.Println("Game exited without error.") // Should only happen on explicit return nil, unlikely here
	}
}
