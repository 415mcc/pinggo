package main

import (
	"flag"
	"fmt"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

const icmpv4ProtocolNum = 1
const icmpv6ProtocolNum = 58

// ID is sent along with Echo requests and responses, by using the PID we know
// that an Echo response is intended for this ping process and not another
// running simulataneously
var icmpEchoID = uint16(os.Getpid() & 0xFFFF)

// The following are dependent on whether we are using ICMP or ICMPv6
var icmpProtocolNum int
var icmpTypeEchoRequest icmp.Type
var icmpTypeEchoReply icmp.Type
var icmpTypeTimeExceeded icmp.Type

// Results from CLI flags
var ttl int
var timeBetweenRequests time.Duration
var requestCount countValue
var bellOnResponse bool
var ipv4Only bool
var ipv6Only bool

// flag.IntVar requires a default value, but for -count the default is off
// countValue is a workaround, implements the flag.Value interface
type countValue struct {
	count   int
	enabled bool
}

func (c *countValue) String() string {
	if c.enabled {
		return string(c.count)
	}
	return "off"
}

func (c *countValue) Set(s string) (err error) {
	c.count, err = strconv.Atoi(s)
	if err != nil && c.count <= 0 {
		return fmt.Errorf("count must be > 0")
	}
	c.enabled = err == nil
	return
}

func init() {
	flag.IntVar(&ttl, "ttl", 64, "TTL `value` for ICMP echo requests (from 1 to 255)")
	flag.DurationVar(&timeBetweenRequests, "wait", time.Second, "Time to wait between sending requests (>= 0.1s)")
	flag.Var(&requestCount, "count", "Stop after sending `n` requests, will wait for response or timeout (> 0) (default off)")
	flag.BoolVar(&bellOnResponse, "beepboop", false, "Beep (using the bell character) when an ICMP echo reply is received")
	flag.BoolVar(&ipv4Only, "4", false, "Use IPv4 only (mututally exclusive with -6)")
	flag.BoolVar(&ipv6Only, "6", false, "Use IPv6 only (mututally exclusive with -4)")

	flag.CommandLine.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options] host\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Options:")
		flag.PrintDefaults()
	}
}

func handleFatalError(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

func handleNonfatalError(err error) {
	fmt.Fprintln(os.Stderr, err)
}

// Validate optional arguments and return host positional argument.
func args() (host string) {
	flag.Parse()
	if (ttl < 1 || 255 < ttl) ||
		(flag.NArg() < 1) ||
		(timeBetweenRequests.Milliseconds() < 100) ||
		(ipv4Only && ipv6Only) {

		flag.CommandLine.Usage()
		os.Exit(2)
	}

	return flag.Arg(0)
}

// Get an IP address that the host resolves to.
// Prioritizes IPv4 address over IPv6, constrained by ipv4Only and ipv6Only.
// Returns the IP, if the IP is IPv4, and potentially an error.
func ipFromHost(host string) (net.IP, bool, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, false, err
	}

	var firstIPv6 net.IP
	for _, ip := range ips {
		if !ipv6Only && ip.To4() != nil {
			return ip, true, nil
		}
		if !ipv4Only && firstIPv6 == nil && ip.To4() == nil {
			firstIPv6 = ip
		}
	}

	if firstIPv6 != nil {
		return firstIPv6, false, nil
	}

	ipString := "IP"
	if ipv4Only {
		ipString = "IPv4"
	} else if ipv6Only {
		ipString = "IPv6"
	}
	return nil, false, fmt.Errorf("No %s addresses found for %s.", ipString, host)
}

func isDueToClosedPacketConn(err error) bool {
	// Doesn't seem to be a better way.
	// golang.org/src/net/error_test.go even has tests to ensure that errors
	// due to a closed network connection nest include this string
	return strings.Contains(err.Error(), "use of closed network connection")
}

// Send ICMP Echo requests every `timeBetweenRequests` until either the done chan
// is closed or if using -count, `requestCount.count` requests have been sent.
func send(conn *icmp.PacketConn, addr net.Addr, pingStatistics *PingStatistics,
	wg *sync.WaitGroup, done <-chan struct{}) {

	marshalledEchoReqIPv4 := func(seq uint16) ([]byte, error) {
		message := icmp.Message{
			Type: icmpTypeEchoRequest, Code: 0,
			Body: &icmp.Echo{
				ID: int(icmpEchoID), Seq: int(seq),
				Data: []byte("This is 56 bytes!! This is 56 bytes!! This is 56 bytes!!"),
			},
		}

		return message.Marshal(nil)
	}

	requestTimeoutPrinter := func(seq uint16) {
		defer wg.Done()

		timer := time.NewTimer(pingStatistics.responseTimeout())

		// block until timer is done, or done chan is closed
		select {
		case <-done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			if pingStatistics.isStillWaitingForResp(seq) {
				fmt.Printf("Request timeout for icmp_seq %v\n", seq)
			}
			timer.Stop()
		}
	}

	defer wg.Done()
	ticker := time.NewTicker(timeBetweenRequests)
	defer ticker.Stop()

	// Loop until done chan is closed or specified number of requests have been sent (-count)
	for seq, i := uint16(0), 0; !requestCount.enabled || i < requestCount.count; seq, i = seq+1, i+1 {
		// Block until ticker fires or done chan closed
		select {
		case <-done:
			return
		case <-ticker.C:
			request, err := marshalledEchoReqIPv4(seq)
			if err != nil {
				handleFatalError(err)
			}

			if _, err := conn.WriteTo(request, addr); err != nil {
				if isDueToClosedPacketConn(err) {
					continue
				}
				handleNonfatalError(err)
				pingStatistics.requestFailed()
				continue
			}

			pingStatistics.addRequest(seq)

			// goroutine to print timeout message if necessary
			wg.Add(1)
			go requestTimeoutPrinter(seq)
		}
	}
}

// Return string with `host (ip)` where host is the result of a reverse lookup.
// If the lookup fails, returns `ip (ip)`.
func hostIPString(ip net.IP) string {
	var hostName string
	addrs, err := net.LookupAddr(ip.String())
	if err == nil && len(addrs) > 0 {
		hostName = addrs[0]
		lastIndex := len(addrs[0]) - 1
		if hostName[lastIndex] == '.' {
			hostName = hostName[:lastIndex]
		}
	} else {
		hostName = ip.String()
	}

	return fmt.Sprintf("%s (%v)", hostName, ip)
}

// Receive, print, and update statistics for Echo responses and Time Exceeded
// messages until done chan is closed.
func receive(conn *icmp.PacketConn, pingStatistics *PingStatistics,
	wg *sync.WaitGroup, done <-chan struct{}) {

	defer wg.Done()
	buffer := make([]byte, 1500)

	isIPv4 := conn.IPv4PacketConn() != nil
	if !isIPv4 && conn.IPv6PacketConn() == nil {
		panic("icmp.PacketConn has neither an IPv4 raw socket nor an IPv6 raw socket.")
	}

	for {
		select {
		case <-done:
			return
		default:
			// Block until response received (or socket closed)
			// Using underlying raw socket (instead of just conn.ReadFrom) to
			// get TTL of Echo responses from IP header
			var n, respTTL int
			var addr net.Addr
			var err error
			if isIPv4 {
				var cm *ipv4.ControlMessage
				n, cm, addr, err = conn.IPv4PacketConn().ReadFrom(buffer)
				if cm != nil {
					respTTL = cm.TTL
				}
			} else {
				var cm *ipv6.ControlMessage
				n, cm, addr, err = conn.IPv6PacketConn().ReadFrom(buffer)
				if cm != nil {
					respTTL = cm.HopLimit
				}
			}
			if err != nil {
				if isDueToClosedPacketConn(err) {
					continue
				}
				handleFatalError(err)
			}

			var ip net.IP = addr.(*net.UDPAddr).IP

			// Parse response
			response, err := icmp.ParseMessage(icmpProtocolNum, buffer[:n])
			if err != nil {
				handleFatalError(err)
			}

			if echoReply, ok := response.Body.(*icmp.Echo); ok &&
				response.Type == icmpTypeEchoReply &&
				echoReply.ID == int(icmpEchoID) {

				rtt, err := pingStatistics.rttMilliseconds(uint16(echoReply.Seq))
				if err != nil {
					handleFatalError(err)
				}

				var optionalBellChar string
				if bellOnResponse {
					optionalBellChar = "\a"
				}
				fmt.Printf("%d bytes from %s: icmp_seq=%d ttl=%d time=%.3f ms%s\n",
					n, hostIPString(ip), echoReply.Seq, respTTL, rtt, optionalBellChar)
			} else if te, ok := response.Body.(*icmp.TimeExceeded); ok &&
				response.Type == icmpTypeTimeExceeded {

				// te.Data is IP Header + ICMP Echo Request from the Echo
				// request that's TTL reached 0. Chop off the IP header and
				// parse the ICMP Echo Request to check the ID and Seq number.
				var headerLength int
				if isIPv4 {
					headerLength = int(te.Data[0]&0x0f) << 2
				} else {
					headerLength = 40
				}

				origReq, err := icmp.ParseMessage(icmpProtocolNum, te.Data[headerLength:])
				if err != nil {
					handleFatalError(err)
				}

				if origEcho, ok := origReq.Body.(*icmp.Echo); ok {
					if origEcho.ID != int(icmpEchoID) {
						fmt.Printf("Got time exceed from %v but bad id.\n", addr)
						return
					}

					err = pingStatistics.requestGotTimeExceeded(uint16(origEcho.Seq))
					if err != nil {
						handleFatalError(err)
					}

					fmt.Printf("From %s icmp_seq=%d Time to live exceeded\n",
						hostIPString(ip), origEcho.Seq)
				}
			}
		}
	}
}

func main() {
	host := args()

	ip, ipIsIPv4, err := ipFromHost(host)
	if err != nil {
		handleFatalError(err)
	}

	var addr net.Addr = &net.UDPAddr{IP: ip, Zone: ""}

	// Setup socket
	var conn *icmp.PacketConn
	if ipIsIPv4 {
		conn, err = icmp.ListenPacket("udp4", "0.0.0.0")
		icmpProtocolNum = icmpv4ProtocolNum
		icmpTypeEchoRequest = ipv4.ICMPTypeEcho
		icmpTypeEchoReply = ipv4.ICMPTypeEchoReply
		icmpTypeTimeExceeded = ipv4.ICMPTypeTimeExceeded
	} else {
		conn, err = icmp.ListenPacket("udp6", "::")
		icmpProtocolNum = icmpv6ProtocolNum
		icmpTypeEchoRequest = ipv6.ICMPTypeEchoRequest
		icmpTypeEchoReply = ipv6.ICMPTypeEchoReply
		icmpTypeTimeExceeded = ipv6.ICMPTypeTimeExceeded
	}
	if err != nil {
		handleFatalError(err)
	}
	defer conn.Close()

	// Have underlying raw socket return TTL in ControlMessage
	if ipIsIPv4 {
		err = conn.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true)
	} else {
		err = conn.IPv6PacketConn().SetControlMessage(ipv6.FlagHopLimit, true)
	}
	if err != nil {
		handleFatalError(err)
	}

	// Set TTL for requests based on CLI flag
	if ipIsIPv4 {
		err = conn.IPv4PacketConn().SetTTL(ttl)
		if err != nil {
			handleFatalError(err)
		}
		err = conn.IPv4PacketConn().SetMulticastTTL(ttl)
		if err != nil {
			handleFatalError(err)
		}
	} else {
		err = conn.IPv6PacketConn().SetHopLimit(ttl)
		if err != nil {
			handleFatalError(err)
		}
		err = conn.IPv6PacketConn().SetMulticastHopLimit(ttl)
		if err != nil {
			handleFatalError(err)
		}
	}

	fmt.Printf("PING %s (%v): 56 data bytes\n", host, ip)

	done := make(chan struct{})
	var pingStatistics *PingStatistics
	var responseDoneChan <-chan struct{}
	if requestCount.enabled {
		pingStatistics, responseDoneChan = NewPingStatisticsWithDoneChan(time.Second,
			requestCount.count)
	} else {
		pingStatistics = NewPingStatistics(time.Second)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go send(conn, addr, pingStatistics, &wg, done)
	go receive(conn, pingStatistics, &wg, done)

	// Let send, receive do their thing, until SIGINT is received (^C) or
	// requestCount.count responses (or timeouts) are received
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	if requestCount.enabled {
		select {
		case <-responseDoneChan:
		case <-sigs:
		}
	} else {
		<-sigs
	}

	// Tell the goroutines to stop, close the connection, wait for them to stop
	close(done)
	conn.Close()
	wg.Wait()

	fmt.Printf("\n--- %v ping statistics ---\n", host)
	pingStatistics.printStatistics()
}
