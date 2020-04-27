# Golang `ping` Implementation
## Installation
```sh
go get github.com/lachm/pinggo
```

## Usage
```sh
pinggo [options] host  # hit Ctrl-C to exit
```

## Features
### RTT & Packet Loss Statistics
Just like the built-in ping utility, the tool outputs percentage packet loss as well as min, max, average, and standard deviation for return trip time. The statistics are printed after `^C` (or when using `-count` after the specified number of requests/responses).

### Adaptive Request Timeouts
The tool will print request timeout messages as echo requests fail to receive replies. The timeout is two times the maximum RTT. If no response has been received yet, the timeout is one second. Try the following:
```sh
pinggo apple.com  # apple.com does not reply to ICMP echo
```

### Set TTL For Echo Requests
You can set the TTL for the echo requests. The tool will report ICMP time exceeded messages. Try the following:
```sh
pinggo -ttl 3 golang.org
```

### IPv6 Support
The tool supports both ICMP and ICMPv6. To force IPv4 use `-4` and to force IPv6 use `-6`. With neither flag, the tool will resolve a hostname to an IPv4 address if available before resolving to an IPv6 address. Try some of the following:
```sh
pinggo ::1
pinggo -6 golang.org
```

### Multicast Support
The tool handles multiple responses to the same sequence number from different hosts gracefully. This entails support for pinging multicast addresses. Try the following:
```sh
pinggo 224.0.0.1
```

### DNS Reverse Lookup
With each Echo response or Time Exceeded response, the tool will try to lookup a name for the IP address. The tool outputs this in the form `host (ip)`. Try the following:
```sh
pinggo github.com
```

### Run Until Specified Number of Echo Requests & Responses
When using the `-count` option, the tool will end execution after the specified number of Echo requests have been sent and all requests have either gotten a response, gotten a time exceeded message, or timed out. Try the following:
```sh
pinggo -count 5 golang.org
```

### Custom Time Between Echo Requests
Users can change the amount of time between Echo requests using the `-wait` option. Try the following:
```sh
pinggo -wait 0.2s -count 10 golang.org
```

### Bell Character Alert For Echo Response
When using the `-beepboop` option, the tool will beep (by printing a bell character) when an Echo response is received. Try the following:
```sh
pinggo -beepboop cloudflare.com
```

### Help Menu
To print the options menu, just run `pinggo` with no hostname argument.
