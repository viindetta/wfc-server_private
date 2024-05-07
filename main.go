package main

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"wwfc/api"
	"wwfc/common"
	"wwfc/gamestats"
	"wwfc/gpcm"
	"wwfc/gpsp"
	"wwfc/logging"
	"wwfc/nas"
	"wwfc/natneg"
	"wwfc/qr2"
	"wwfc/sake"
	"wwfc/serverbrowser"

	"github.com/logrusorgru/aurora/v3"
)

var config = common.GetConfig()

func main() {
	logging.SetLevel(*config.LogLevel)

	args := os.Args[1:]

	// Separate frontend and backend into two separate processes.
	// This is to allow restarting the backend without closing all connections.

	// Start the backend instead of the frontend if the first argument is "backend"
	if len(args) > 0 && args[0] == "backend" {
		backendMain(len(args) > 1 && args[1] == "reload")
	} else if len(args) > 0 && args[0] == "cmd" {
		handleCommand(args[1:])
	} else {
		frontendMain(len(args) > 0 && args[0] == "skipbackend")
	}
}

type RPCPacket struct {
	Server  string
	Index   uint64
	Address string
	Data    []byte
}

// backendMain starts all the servers and creates an RPC server to communicate with the frontend
func backendMain(reload bool) {
	if err := logging.SetOutput(config.LogOutput); err != nil {
		logging.Error("BACKEND", err)
	}

	rpc.Register(&RPCPacket{})
	address := "localhost:29999"

	l, err := net.Listen("tcp", address)
	if err != nil {
		logging.Error("BACKEND", "Failed to listen on", aurora.BrightCyan(address))
		os.Exit(1)
	}

	common.ConnectFrontend()

	wg := &sync.WaitGroup{}
	actions := []func(bool){nas.StartServer, gpcm.StartServer, qr2.StartServer, gpsp.StartServer, serverbrowser.StartServer, sake.StartServer, natneg.StartServer, api.StartServer, gamestats.StartServer}
	wg.Add(len(actions))
	for _, action := range actions {
		go func(ac func(bool)) {
			defer wg.Done()
			ac(reload)
		}(action)
	}

	// Wait for all servers to start
	wg.Wait()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				logging.Error("BACKEND", "Failed to accept connection on", aurora.BrightCyan(address))
				continue
			}

			go rpc.ServeConn(conn)
		}
	}()

	logging.Notice("BACKEND", "Listening on", aurora.BrightCyan(address))

	// Prevent application from exiting
	select {}
}

// RPCPacket.NewConnection is called by the frontend to notify the backend of a new connection
func (r *RPCPacket) NewConnection(args RPCPacket, _ *struct{}) error {
	switch args.Server {
	case "serverbrowser":
		serverbrowser.NewConnection(args.Index, args.Address)
	case "gpcm":
		gpcm.NewConnection(args.Index, args.Address)
	case "gpsp":
		gpsp.NewConnection(args.Index, args.Address)
	case "gamestats":
		gamestats.NewConnection(args.Index, args.Address)
	}

	return nil
}

// RPCPacket.HandlePacket is called by the frontend to forward a packet to the backend
func (r *RPCPacket) HandlePacket(args RPCPacket, _ *struct{}) error {
	switch args.Server {
	case "serverbrowser":
		serverbrowser.HandlePacket(args.Index, args.Data, args.Address)
	case "gpcm":
		gpcm.HandlePacket(args.Index, args.Data)
	case "gpsp":
		gpsp.HandlePacket(args.Index, args.Data)
	case "gamestats":
		gamestats.HandlePacket(args.Index, args.Data)
	}

	return nil
}

// RPCPacket.closeConnection is called by the frontend to notify the backend of a closed connection
func (r *RPCPacket) CloseConnection(args RPCPacket, _ *struct{}) error {
	switch args.Server {
	case "serverbrowser":
		serverbrowser.CloseConnection(args.Index)
	case "gpcm":
		gpcm.CloseConnection(args.Index)
	case "gpsp":
		gpsp.CloseConnection(args.Index)
	case "gamestats":
		gamestats.CloseConnection(args.Index)
	}

	return nil
}

// RPCPacket.Shutdown is called by the frontend to shutdown the backend
func (r *RPCPacket) Shutdown(_ struct{}, _ *struct{}) error {
	wg := &sync.WaitGroup{}
	actions := []func(){nas.Shutdown, gpcm.Shutdown, qr2.Shutdown, gpsp.Shutdown, serverbrowser.Shutdown, sake.Shutdown, natneg.Shutdown, api.Shutdown, gamestats.Shutdown}
	wg.Add(len(actions))
	for _, action := range actions {
		go func(ac func()) {
			defer wg.Done()
			ac()
		}(action)
	}

	wg.Wait()

	os.Exit(0)
	return nil
}

type serverInfo struct {
	rpcName  string
	protocol string
	port     int
}

type RPCFrontendPacket struct {
	Server string
	Index  uint64
	Data   []byte
}

var (
	rpcClient *rpc.Client

	rpcMutex     sync.Mutex
	rpcBusyCount sync.WaitGroup

	connections = map[string]map[uint64]net.Conn{}
)

// frontendMain starts the backend process and communicates with it using RPC
func frontendMain(skipBackend bool) {
	// Don't allow the frontend to output to a file (there's no reason to)
	logOutput := config.LogOutput
	if logOutput == "StdOutAndFile" {
		logOutput = "StdOut"
	}

	if err := logging.SetOutput(logOutput); err != nil {
		logging.Error("FRONTEND", err)
	}

	rpcMutex.Lock()

	startFrontendServer()

	if !skipBackend {
		go startBackendProcess(false, true)
	} else {
		go waitForBackend()
	}

	servers := []serverInfo{
		{rpcName: "serverbrowser", protocol: "tcp", port: 28910},
		{rpcName: "gpcm", protocol: "tcp", port: 29900},
		{rpcName: "gpsp", protocol: "tcp", port: 29901},
		{rpcName: "gamestats", protocol: "tcp", port: 29920},
	}

	for _, server := range servers {
		connections[server.rpcName] = map[uint64]net.Conn{}
		go frontendListen(server)
	}

	// Prevent application from exiting
	select {}
}

// startFrontendServer starts the frontend RPC server.
func startFrontendServer() {
	rpc.Register(&RPCFrontendPacket{})
	address := "localhost:29998"

	l, err := net.Listen("tcp", address)
	if err != nil {
		logging.Error("FRONTEND", "Failed to listen on", aurora.BrightCyan(address))
		os.Exit(1)
	}

	logging.Notice("FRONTEND", "Listening on", aurora.BrightCyan(address))

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				logging.Error("FRONTEND", "Failed to accept connection on", aurora.BrightCyan(address))
				continue
			}

			go rpc.ServeConn(conn)
		}
	}()
}

// startBackendProcess starts the backend process and (optionally) waits for the RPC server to start.
// If wait is true, expects the RPC mutex to be locked.
func startBackendProcess(reload bool, wait bool) {
	exe, err := os.Executable()
	if err != nil {
		logging.Error("FRONTEND", "Failed to get executable path:", err)
		os.Exit(1)
	}

	logging.Info("FRONTEND", "Running from", aurora.BrightCyan(exe))

	var cmd *exec.Cmd
	if reload {
		cmd = exec.Command(exe, "backend", "reload")
	} else {
		cmd = exec.Command(exe, "backend")
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		logging.Error("FRONTEND", "Failed to start backend process:", err)
		os.Exit(1)
	}

	if wait {
		waitForBackend()
	}
}

// waitForBackend waits for the backend to start.
// Expects the RPC mutex to be locked.
func waitForBackend() {
	for {
		client, err := rpc.Dial("tcp", "localhost:29999")
		if err == nil {
			rpcClient = client
			rpcMutex.Unlock()
			return
		}
	}
}

// frontendListen listens on the specified port and forwards each packet to the backend
func frontendListen(server serverInfo) {
	address := *config.GameSpyAddress + ":" + strconv.Itoa(server.port)
	l, err := net.Listen(server.protocol, address)
	if err != nil {
		logging.Error("FRONTEND", "Failed to listen on", aurora.BrightCyan(address))
		return
	}

	logging.Notice("FRONTEND", "Listening on", aurora.BrightCyan(address), "for", aurora.BrightCyan(server.rpcName))

	// Increment by 1 for each connection, never decrement. Unlikely to overflow but it doesn't matter if it does.
	count := uint64(0)

	for {
		conn, err := l.Accept()
		if err != nil {
			logging.Error("FRONTEND", "Failed to accept connection on", aurora.BrightCyan(address))
			continue
		}

		if server.protocol == "tcp" {
			err := conn.(*net.TCPConn).SetKeepAlive(true)
			if err != nil {
				logging.Warn("FRONTEND", "Unable to set keepalive", err.Error())
			}
		}

		count++

		go handleConnection(server, conn, count)
	}
}

// handleConnection forwards packets between the frontend and backend
func handleConnection(server serverInfo, conn net.Conn, index uint64) {
	defer conn.Close()

	rpcMutex.Lock()
	rpcBusyCount.Add(1)
	connections[server.rpcName][index] = conn
	rpcMutex.Unlock()

	err := rpcClient.Call("RPCPacket.NewConnection", RPCPacket{Server: server.rpcName, Index: index, Address: conn.RemoteAddr().String(), Data: []byte{}}, nil)

	rpcBusyCount.Done()

	if err != nil {
		logging.Error("FRONTEND", "Failed to forward new connection to backend:", err)

		rpcMutex.Lock()
		delete(connections[server.rpcName], index)
		rpcMutex.Unlock()
		return
	}

	for {
		buffer := make([]byte, 1024)
		n, err := conn.Read(buffer)
		if err != nil {
			break
		}

		if n == 0 {
			continue
		}

		rpcMutex.Lock()
		rpcBusyCount.Add(1)
		rpcMutex.Unlock()

		// Forward the packet to the backend
		err = rpcClient.Call("RPCPacket.HandlePacket", RPCPacket{Server: server.rpcName, Index: index, Address: conn.RemoteAddr().String(), Data: buffer[:n]}, nil)

		rpcBusyCount.Done()

		if err != nil {
			logging.Error("FRONTEND", "Failed to forward packet to backend:", err)
			if err == rpc.ErrShutdown {
				os.Exit(1)
			}
			break
		}
	}

	rpcMutex.Lock()
	rpcBusyCount.Add(1)
	delete(connections[server.rpcName], index)
	rpcMutex.Unlock()

	err = rpcClient.Call("RPCPacket.CloseConnection", RPCPacket{Server: server.rpcName, Index: index, Address: conn.RemoteAddr().String(), Data: []byte{}}, nil)

	rpcBusyCount.Done()

	if err != nil {
		logging.Error("FRONTEND", "Failed to forward close connection to backend:", err)
		if err == rpc.ErrShutdown {
			os.Exit(1)
		}
	}
}

var ErrBadIndex = errors.New("incorrect connection index")

// RPCFrontendPacket.SendPacket is called by the backend to send a packet to a connection
func (r *RPCFrontendPacket) SendPacket(args RPCFrontendPacket, _ *struct{}) error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()

	conn, ok := connections[args.Server][args.Index]
	if !ok {
		return ErrBadIndex
	}

	_, err := conn.Write(args.Data)
	return err
}

// RPCFrontendPacket.CloseConnection is called by the backend to close a connection
func (r *RPCFrontendPacket) CloseConnection(args RPCFrontendPacket, _ *struct{}) error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()

	conn, ok := connections[args.Server][args.Index]
	if !ok {
		return ErrBadIndex
	}

	delete(connections[args.Server], args.Index)
	return conn.Close()
}

// RPCFrontendPacket.ReloadBackend is called by an external program to reload the backend
func (r *RPCFrontendPacket) ReloadBackend(_ struct{}, _ *struct{}) error {
	r.ShutdownBackend(struct{}{}, &struct{}{})

	// Unlocks the mutex locked by ShutdownBackend
	startBackendProcess(true, false)

	return nil
}

// RPCFrontendPacket.ShutdownBackend is called by an external program to shutdown the backend
func (r *RPCFrontendPacket) ShutdownBackend(_ struct{}, _ *struct{}) error {
	// Lock indefinitely
	rpcMutex.Lock()

	rpcBusyCount.Wait()

	err := rpcClient.Call("RPCPacket.Shutdown", struct{}{}, nil)
	if err != nil && !strings.Contains(err.Error(), "An existing connection was forcibly closed by the remote host.") {
		logging.Error("FRONTEND", "Failed to reload backend:", err)
	}

	err = rpcClient.Close()
	if err != nil {
		logging.Error("FRONTEND", "Failed to close RPC client:", err)
	}

	go waitForBackend()

	return nil
}

// handleCommand is used to send a command to the backend
func handleCommand(args []string) {
	if len(args) < 2 {
		fmt.Printf("Usage: %s cmd <f|b> <command...>\n", os.Args[0])
		return
	}

	var client *rpc.Client
	var err error

	if args[0] == "f" {
		client, err = rpc.Dial("tcp", "localhost:29998")
	} else if args[0] == "b" {
		client, err = rpc.Dial("tcp", "localhost:29999")
	} else {
		fmt.Printf("Unknown command type: '%s', please supply 'f' or 'b' (for frontend or backend)\n", args[0])
		return
	}

	if err != nil {
		fmt.Println("Failed to connect to RPC server:", err)
		return
	}

	defer client.Close()

	if args[0] == "b" {
		fmt.Printf("Unknown backend command: '%s'\n", args[1])
	} else {
		if args[1] == "backend" {
			if len(args) > 2 && args[2] == "shutdown" {
				err = client.Call("RPCFrontendPacket.ShutdownBackend", struct{}{}, nil)
			} else {
				err = client.Call("RPCFrontendPacket.ReloadBackend", struct{}{}, nil)
			}
		} else {
			fmt.Printf("Unknown frontend command: '%s'\n", args[1])
		}
	}

	if err != nil {
		fmt.Println("Failed to send command:", err)
	}
}
