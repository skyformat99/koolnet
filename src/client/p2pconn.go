package client

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync/atomic"
	"time"

	"github.com/icholy/killable"

	common "../common"
)

type P2pConn struct {
	killable.Killable
	tcpConn   net.Conn
	innerPort int
	tcpPort   int
	isProxy   bool
	rLen      int32
	wLen      int32

	//For write buffer
	rlist []*common.MsgBuf

	//For read stream control
	snd  int32
	tick int32
	wait int32
	avg  int32

	hdr  common.MsgHdr
	in   chan *common.MsgBuf
	wMsg chan *common.MsgBuf
}

func (p *P2pConn) RunFor(p2pc *P2pClient, c *Client, updateChan *time.Ticker) error {
	select {
	case <-p2pc.Dying():
		return common.ErrPromisePDying
	case <-p.Dying():
		//Maybe not run, if other place return error
		p.GoDying(p2pc, c)
		return killable.ErrDying
	case mb, ok := <-p.in:
		if ok {
			defer mb.Free()

			if mb.Type != common.MsgTypeSynOk {
				if len(p.rlist) > 64 {
					common.Warn("tcpWrite block forever, kill it")
					p.GoDying(p2pc, c)
					return common.ErrMsgWrite
				}
				p.rlist = append(p.rlist, mb.Dup())
				p.writeAll()
			} else {
				//Always requestor
				common.Info("go tcpReadLoop")
				go p.tcpReadLoop(p2pc, c)
			}
		}
	case <-updateChan.C:
		p.writeAll()
	}

	return nil
}

//Write all buffer to tcp write, not block hear
func (p *P2pConn) writeAll() {
	if len(p.rlist) > 0 {
		for pos, mb := range p.rlist {
			defer mb.Free()

			select {
			case p.wMsg <- mb.Dup():
				//write ok, set pos to nil
				p.rlist[pos] = nil
			default:
				//blocked, remove [0, pos) and return. The mb is not free hear
				mb.Dup()
				p.rlist = p.rlist[pos:]
				//common.Warn("write pos blocked", pos)
				return
			}
		}
		//write complete, remove all
		p.rlist = p.rlist[len(p.rlist):]
	}
}

func (p *P2pConn) Init(p2pc *P2pClient, c *Client) error {
	p.hdr = common.MsgHdr{
		Type: common.MsgTypeData,
		Addr: uint8(c.clientId),
		Port: uint16(p.tcpPort),
		Seq:  uint16(p.innerPort),
	}

	go p.tcpWriteLoop(p2pc, c)

	mb := common.NewMsgBuf()
	defer mb.Free()
	//log.Println("Init p2pconn", mb.Id)

	var synHdr common.MsgHdr
	bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
	if p.isProxy {
		go p.tcpReadLoop(p2pc, c)

		//Tell remote that we are ready
		synHdr = common.MsgHdr{
			Type: common.MsgTypeSynOk,
			Port: uint16(p.tcpPort),
			Seq:  uint16(p.innerPort),
		}
	} else {
		//Send MsgTypeSync
		synHdr = common.MsgHdr{
			Type: common.MsgTypeSyn,
			Port: uint16(p.tcpPort),
			Seq:  uint16(p.innerPort),
		}
	}

	binary.Write(bf, binary.BigEndian, synHdr)
	copy(mb.GetBuf(), bf.Bytes())
	mb.Size = common.MsgHdrSize
	select {
	case <-p2pc.Dying():
		return common.ErrMsgKilled
	case p2pc.wMsg <- mb.Dup():
	}
	return nil
}

func (p *P2pConn) Run(p2pc *P2pClient, c *Client) error {
	if err := p.Init(p2pc, c); err != nil {
		return err
	}

	updateChan := time.NewTicker(UPADTE_TICK * 5)
	defer updateChan.Stop()

	for {
		if err := p.RunFor(p2pc, c, updateChan); err != nil {
			common.Info("P2pConn error", err)
			return err
		}
	}

	return nil
}

func (p *P2pConn) GoDying(p2pc *P2pClient, c *Client) {
	common.NewPromise(p2pc).Then(func(pt common.PromiseTask, arg interface{}) (common.PromiseTask, interface{}, error) {
		p2pc.RemoveConn(p)
		return nil, nil, nil
	}).Resolve(p2pc, p)
}

func (p *P2pConn) Close() {
	p.tcpConn.Close()
	close(p.in)
	close(p.wMsg)
	for msg := range p.in {
		msg.Free()
	}
	for msg := range p.wMsg {
		msg.Free()
	}
	if len(p.rlist) > 0 {
		for _, msg := range p.rlist {
			msg.Free()
		}
		common.Warn("closed but rlist still have buffers", len(p.rlist))
		p.rlist = p.rlist[len(p.rlist):]
	}

	//common.Info("p2pconn closed recv", atomic.LoadInt32(&p.rLen), atomic.LoadInt32(&p.wLen))
}

func (p *P2pConn) tcpReadLoopFor(p2pc *P2pClient, c *Client) error {
	//TODO how to named this var?
	const (
		A = 80
		B = 10
	)

	snd1 := atomic.LoadInt32(&p2pc.waitSend) * A

	tick_diff := iclock()/B - p.tick
	diff := snd1 - p.snd

	if tick_diff > 0 {
		//Update avg for stream control
		p.avg = int32((3*p.avg + diff/tick_diff) / 4)
		if p.avg < 0 {
			p.avg = 0
		}
	} else {
		p.avg = 0
	}
	if diff < 0 {
		//Send ok, so reset p.wait = 0
		p.wait = 0
	}
	common.Info("avg,diff,tick_diff,wait", p.avg, diff, tick_diff, p.wait)
	if p.avg > 10 {
		p.wait += p.avg

		select {
		case <-p.Dying():
			break
		case <-time.After(time.Millisecond * time.Duration(p.avg)):
		}
	}
	p.snd = snd1
	p.tick = iclock() / B

	mb := common.NewMsgBuf()
	defer mb.Free()
	//log.Println("tcpReadLoopFor", mb.Id)

	//Setup hdr
	bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
	binary.Write(bf, binary.BigEndian, p.hdr)
	copy(mb.GetBuf(), bf.Bytes())

	n, err := p.tcpConn.Read(mb.GetBuf()[common.MsgHdrSize:])

	//wait too long, just kill myself
	if p.wait > 10000 || nil != err {
		bf.Reset()
		p.hdr.Type = common.MsgTypeFin
		binary.Write(bf, binary.BigEndian, p.hdr)
		copy(mb.GetBuf(), bf.Bytes())
		mb.Size = common.MsgHdrSize
		//common.Warn("tcpRead error", p.wait, err)

		select {
		//Already dying, ignore Fin message
		case <-p.Dying():
		case <-p2pc.Dying():
		case p2pc.wMsg <- mb.Dup():
		}
		p.Kill(common.ErrMsgRead)
		return common.ErrMsgRead
	} else {
		mb.Size = n + common.MsgHdrSize
		atomic.AddInt32(&p.rLen, int32(n))
		select {
		case <-p2pc.Dying():
			return common.ErrMsgKilled
		case p2pc.wMsg <- mb.Dup():
		}
		return nil
	}
}

func (p *P2pConn) tcpReadLoop(p2pc *P2pClient, c *Client) {
	//TODO Use fix timeout
	//p.tcpConn.SetReadDeadline(time.Now().Add(common.UdpP2pPingTimeout))
	p.avg = 10
	p.tick = iclock() / 10

	for {
		if err := p.tcpReadLoopFor(p2pc, c); err != nil {
			return
		}
	}
}

func (p *P2pConn) tcpWriteLoopFor(p2pc *P2pClient, c *Client) error {
	select {
	case mb, ok := <-p.wMsg:
		if ok {
			defer mb.Free()

			switch mb.Type {
			case common.MsgTypeData:
				size := mb.Size
				wsize := 0
			SEND_LOOP:
				for {
					if n, err := p.tcpConn.Write(mb.GetReal()[wsize:]); err == nil && n == (size-wsize) {
						wsize += n
						break SEND_LOOP
					} else if err == nil {
						wsize += n
					} else {
						//Response fin message
						p.hdr.Type = common.MsgTypeFin
						rmb := common.NewMsgBuf()
						defer rmb.Free()
						//log.Println("send_loop", rmb.Id)

						bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
						binary.Write(bf, binary.BigEndian, p.hdr)
						copy(rmb.GetBuf(), bf.Bytes())
						rmb.Size = common.MsgHdrSize
						select {
						case <-p2pc.Dying():
						case p2pc.wMsg <- rmb.Dup():
						}

						p.Kill(common.ErrMsgWrite)
						return common.ErrMsgWrite
					}
				}

				atomic.AddInt32(&p.wLen, int32(wsize))

			case common.MsgTypeFin, common.MsgTypeSynErr:
				//common.Info("kill by remote", atomic.LoadInt32(&p.wLen))
				p.Kill(common.ErrMsgWrite)
				return common.ErrMsgWrite
			}

		} else {
			//Killed, just return error
			//common.Warn("tcpwrite error")
			return common.ErrMsgWrite
		}
	case <-p.Dying():
		//Killed, just return error
		return common.ErrMsgWrite
	}
	return nil
}

func (p *P2pConn) tcpWriteLoop(p2pc *P2pClient, c *Client) {
	for {
		if err := p.tcpWriteLoopFor(p2pc, c); err != nil {
			return
		}
	}
}
