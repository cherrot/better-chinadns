package gochinadns

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

func lookupInServers(
	ctx context.Context, cancel context.CancelFunc, result chan<- *dns.Msg, req *dns.Msg,
	servers []Resolver, waitInterval time.Duration, lookup LookupFunc,
) {
	defer cancel()
	if len(servers) == 0 {
		return
	}
	logger := logrus.WithField("question", questionString(&req.Question[0]))

	// TODO: replace ticker by ratelimit
	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()
	queryNext := make(chan struct{}, len(servers))
	queryNext <- struct{}{}
	var wg sync.WaitGroup

	doLookup := func(server Resolver) {
		defer wg.Done()
		logger := logger.WithField("server", server.GetAddr())

		reply, rtt, err := lookup(req.Copy(), server)
		if err != nil {
			queryNext <- struct{}{}
			return
		}

		select {
		case result <- reply:
			logger.Debug("Query RTT: ", rtt)
		default:
		}
		cancel()
	}

LOOP:
	for _, server := range servers {
		select {
		case <-ctx.Done():
			break LOOP
		case <-queryNext:
			wg.Add(1)
			go doLookup(server)
		case <-ticker.C:
			wg.Add(1)
			go doLookup(server)
		}
	}

	wg.Wait()
}

// Serve serves DNS request.
func (s *Server) Serve(w dns.ResponseWriter, req *dns.Msg) {
	// Its client's responsibility to close this conn.
	// defer w.Close()
	var reply *dns.Msg

	start := time.Now()
	qName := req.Question[0].Name
	logger := logrus.WithField("question", questionString(&req.Question[0]))

	if s.DomainBlacklist.Contain(qName) {
		reply = new(dns.Msg)
		reply.SetReply(req)
		_ = w.WriteMsg(reply)
		return
	}

	ctx, cancel := context.WithCancel(context.TODO())
	uctx, ucancel := context.WithCancel(ctx)
	tctx, tcancel := context.WithCancel(ctx)
	go func() {
		<-uctx.Done()
		<-tctx.Done()
		cancel()
	}()

	s.normalizeRequest(req)

	trusted := make(chan *dns.Msg, 1)
	untrusted := make(chan *dns.Msg, 1)
	go lookupInServers(tctx, tcancel, trusted, req, s.TrustedServers, s.Delay, s.Lookup)
	if !s.DomainPolluted.Contain(qName) {
		go lookupInServers(uctx, ucancel, untrusted, req, s.UntrustedServers, s.Delay, s.lookupNormal)
	} else {
		ucancel()
	}

	select {
	case rep := <-untrusted:
		reply = s.processReply(ctx, logger, rep, trusted, s.processUntrustedAnswer)
	case rep := <-trusted:
		reply = s.processReply(ctx, logger, rep, untrusted, s.processTrustedAnswer)
	case <-ctx.Done():
	}
	// notify lookupInServers to quit.
	cancel()

	if reply != nil {
		// https://github.com/miekg/dns/issues/216
		reply.Compress = true
	} else {
		reply = new(dns.Msg)
		reply.SetReply(req)
	}

	_ = w.WriteMsg(reply)
	logger.Debug("SERVING RTT: ", time.Since(start))
}

func (s *Server) normalizeRequest(req *dns.Msg) {
	req.RecursionDesired = true
	if !s.TCPOnly {
		setUDPSize(req, uint16(s.UDPMaxSize))
	}
}

func (s *Server) processReply(
	ctx context.Context, logger *logrus.Entry, rep *dns.Msg, other <-chan *dns.Msg,
	process func(context.Context, *logrus.Entry, *dns.Msg, net.IP, <-chan *dns.Msg) *dns.Msg,
) (reply *dns.Msg) {
	reply = rep
	for i, rr := range rep.Answer {
		switch answer := rr.(type) {
		case *dns.A:
			return process(ctx, logger, rep, answer.A, other)
		case *dns.AAAA:
			return process(ctx, logger, rep, answer.AAAA, other)
		case *dns.CNAME:
			if i < len(rep.Answer)-1 {
				continue
			}
			logger.Debug("CNAME to ", answer.Target)
			return
		default:
			return
		}
	}
	return
}

func (s *Server) processUntrustedAnswer(ctx context.Context, logger *logrus.Entry, rep *dns.Msg, answer net.IP, trusted <-chan *dns.Msg) (reply *dns.Msg) {
	reply = rep
	logger = logger.WithField("answer", answer)

	hit, err := s.IPBlacklist.Contains(answer)
	if err != nil {
		logger.WithError(err).Error("Blacklist CIDR error.")
	}
	if hit {
		logger.Debug("Answer hit blacklist. Wait for trusted reply.")
	} else {
		contain, err := s.ChinaCIDR.Contains(answer)
		if err != nil {
			logger.WithError(err).Error("CIDR error.")
		}
		if contain {
			logger.Debug("Answer belongs to China. Use it.")
			return
		}
		logger.Debug("Answer is overseas. Wait for trusted reply.")
	}

	select {
	case rep := <-trusted:
		reply = s.processReply(ctx, logger, rep, nil, s.processTrustedAnswer)
	case <-ctx.Done():
		logger.Warn("No trusted reply. Use this as fallback.")
	}
	return
}

func (s *Server) processTrustedAnswer(ctx context.Context, logger *logrus.Entry, rep *dns.Msg, answer net.IP, untrusted <-chan *dns.Msg) (reply *dns.Msg) {
	reply = rep
	logger = logger.WithField("answer", answer)

	hit, err := s.IPBlacklist.Contains(answer)
	if err != nil {
		logger.WithError(err).Error("Blacklist CIDR error.")
	}
	if hit {
		logger.Debug("Answer hit blacklist. Wait for trusted reply.")
	} else {
		if !s.Bidirectional {
			logger.Debug("Answer is trusted. Use it.")
			return
		}

		contain, err := s.ChinaCIDR.Contains(answer)
		if err != nil {
			logger.WithError(err).Error("CIDR error.")
		}
		if !contain {
			logger.Debug("Answer is trusted and overseas. Use it.")
			return
		}
		logger.Debug("Answer may not be the nearest. Wait for untrusted reply.")
	}

	select {
	case rep := <-untrusted:
		reply = s.processReply(ctx, logger, rep, nil, s.processUntrustedAnswer)
	case <-ctx.Done():
		logger.Debug("No untrusted reply. Use this as fallback.")
	}
	return
}
