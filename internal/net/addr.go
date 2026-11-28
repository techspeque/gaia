package net

import stdnet "net"

// addrKeepingListener returns connections that report the remote and local
// addresses read when they were accepted. tsnet's netstack connections
// report a nil RemoteAddr once the peer has reset the connection, which a
// failed TLS handshake does, so http.Server would log the handshake error
// "from <nil>" and the warning could not be tied to a device.
type addrKeepingListener struct{ stdnet.Listener }

func (l addrKeepingListener) Accept() (stdnet.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &addrKeepingConn{Conn: c, remote: c.RemoteAddr(), local: c.LocalAddr()}, nil
}

type addrKeepingConn struct {
	stdnet.Conn
	remote, local stdnet.Addr
}

func (c *addrKeepingConn) RemoteAddr() stdnet.Addr {
	if c.remote != nil {
		return c.remote
	}
	return c.Conn.RemoteAddr()
}

func (c *addrKeepingConn) LocalAddr() stdnet.Addr {
	if c.local != nil {
		return c.local
	}
	return c.Conn.LocalAddr()
}
