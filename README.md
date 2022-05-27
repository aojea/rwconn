# rwconn

Create a golang net.Conn using a Reader and a Writer.

The connection created implements Deadlines, that are used to stop a Read on a net.Conn without closing the connection.

This is really useful when you want to use the connection to send http traffic, since the net/http library uses the deadline
the cancel reads without closing the connections, per example, for http.Hijacking

https://groups.google.com/g/golang-nuts/c/VPVWFrpIEyo/m/d5CdnIsPAwAJ
