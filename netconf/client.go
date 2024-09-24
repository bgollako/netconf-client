package netconf

import (
	"errors"
	"net"
	"strconv"
	"time"
)

type Client interface {
	// Starts the NETCONF client with the given configuration.
	// The client can be created by calling the NewClient() method.
	// Answers any call homes on the port provided on given port.
	Start() error
	// Returns a channel over which call-homed NETCONF sessions.
	// are returned. This channel must be consumed from to prevent
	// new routines from being spawned.
	Sessions() <-chan Session
	// Closes the NETCONF client and all the sessions associated with it.
	Close()
}

type Version int

const (
	// NETCONF version 1.0 && 1.1
	Netconf_Version_1_0_1_1 = iota
	// NETCONF version 1.1
	Netconf_Version_1_1
	// NETCONF version 1.0
	Netconf_Version_1_0
)

type Config struct {
	// The size of the hello message size that is coming from the NETCONF server
	// Defaults to 4096 bytes if unspecified, must be large enough to read the
	// hello msg from the NETCONF server.
	HelloSize int
	// Port on which the NETCONF client listens to incoming requests
	Port int
	// Delay to allow the NETCONF server to switch to SSH
	// Defaults to 2 seconds if unspecified.
	// Delay will be removed if set to 0 value.
	Delay time.Duration
	// function to get credentials for given call-home
	// the hello message is sent across as response to data
	GetCredentials func(hello []byte) (username, password string)
	// channel size to recieve rpcs to execute
	// default size is 10
	WriteChannelSize int
	// Size of channel that houses created sessions
	// as they are returned to the caller.
	// Defaults to 10 if unspecified
	ReturnSessionChannelSize int
}

// NETCONF Client that accepts call-home
type client struct {
	// NETCONF Client Configuration
	config *Config
	// map of internal sessions to various endpoints
	m map[*session]bool
	// channel on which the client returns the sessions to various
	// endpoints
	ch chan Session
}

func NewClient(c *Config) (Client, error) {
	if c.Port == 0 {
		return nil, errors.New("invalid port no")
	}

	if c.HelloSize == 0 {
		c.HelloSize = defaultHelloSize
	}

	if c.Delay == time.Duration(0) {
		c.Delay = defaultSwitchDelay
	}
	if c.GetCredentials == nil {
		return nil, errors.New("unimplemented getCredentials callback")
	}
	if c.WriteChannelSize == 0 {
		c.WriteChannelSize = defaultWriteChannelSize
	}
	if c.ReturnSessionChannelSize == 0 {
		c.ReturnSessionChannelSize = defaultReturnSessionChannelSize
	}
	return &client{
		config: c,
		m:      make(map[*session]bool),
		ch:     make(chan Session, c.ReturnSessionChannelSize),
	}, nil
}

// Starts a TCP server on the given port and spawns a new routine
// to answer every call home
func (c *client) Start() error {
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(c.config.Port))
	if err != nil {
		return err
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}

		s := &session{
			config:    c.config,
			conn:      conn,
			writeChan: make(chan []byte, c.config.WriteChannelSize),
			idmap:     make(map[int]chan []byte),
		}
		c.m[s] = true

		if len(c.ch) < c.config.ReturnSessionChannelSize {
			c.ch <- s
		} else {
			go func() {
				c.ch <- s
			}()
		}

		go s.handleConn()
	}
}

// Returns a channel over which created sessions are received
func (c *client) Sessions() <-chan Session {
	return c.ch
}

// Closes the client and all the underlying NETCONF sessions
func (c *client) Close() {
	for session := range c.m {
		session.Close()
	}
}
