package sip

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/zaf/g711"
)

const (
	wavFilePath       = "audio/announcement.wav"
	rtpSampleRate     = 8000 // PCMUのサンプルレート
	rtpPacketDuration = 20   // 20msのRTPパケット
)

// App は、単一のガイダンスコールセッションを処理します。
type App struct {
	server         *SIPServer
	inviteTx       ServerTransaction
	peerConnection *webrtc.PeerConnection
	audioTrack     *webrtc.TrackLocalStaticRTP
	done           chan struct{} // PeerConnectionが閉じたときに通知するチャネル

	// BYEを送信するためのダイアログ状態
	callID       string
	localTag     string
	remoteTag    string
	remoteTarget string // INVITEのContactヘッダーからのURI
	cseq         uint32
	ssrc         webrtc.SSRC
}

// NewApp は、新しいガイダンスアプリケーションインスタンスを作成します。
func NewApp(server *SIPServer, tx ServerTransaction) *App {
	return &App{
		server:   server,
		inviteTx: tx,
		cseq:     1, // BYEの初期CSeqは1ですが、INVITEから取得する必要があります
		done:     make(chan struct{}),
	}
}

// Run は、ガイダンスコールのロジックを開始します。
func (a *App) Run() {
	req := a.inviteTx.OriginalRequest()
	a.callID = req.GetHeader("Call-ID")
	log.Printf("GuidanceApp: Starting for call %s", a.callID)

	fromHeader := req.GetHeader("From")
	a.localTag = GetTag(fromHeader)
	contactHeader := req.GetHeader("Contact")
	if start := strings.Index(contactHeader, "<"); start != -1 {
		if end := strings.Index(contactHeader, ">"); end > start {
			a.remoteTarget = contactHeader[start+1 : end]
		}
	}
	if a.remoteTarget == "" {
		log.Printf("GuidanceApp: Could not extract URI from Contact header: %s", contactHeader)
		a.inviteTx.Respond(BuildResponse(400, "Bad Request", req, nil))
		return
	}

	cseqStr := req.GetHeader("CSeq")
	if parts := strings.Fields(cseqStr); len(parts) >= 1 {
		if c, err := strconv.ParseUint(parts[0], 10, 32); err == nil {
			atomic.StoreUint32(&a.cseq, uint32(c))
		}
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: rtpSampleRate},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		log.Printf("GuidanceApp: Error registering codec: %v", err)
		a.inviteTx.Respond(BuildResponse(500, "Server Internal Error", req, nil))
		return
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetEphemeralUDPPortRange(10000, 11000)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settingEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Printf("GuidanceApp: Error creating PeerConnection: %v", err)
		a.inviteTx.Respond(BuildResponse(500, "Server Internal Error", req, nil))
		return
	}
	a.peerConnection = pc
	defer pc.Close()

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU}, "audio", "pion")
	if err != nil {
		log.Printf("GuidanceApp: Error creating audio track: %v", err)
		a.inviteTx.Respond(BuildResponse(500, "Server Internal Error", req, nil))
		return
	}
	rtpSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		log.Printf("GuidanceApp: Error adding audio track: %v", err)
		a.inviteTx.Respond(BuildResponse(500, "Server Internal Error", req, nil))
		return
	}
	a.audioTrack = audioTrack
	a.ssrc = rtpSender.GetParameters().Encodings[0].SSRC

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(req.Body)}
	if err := pc.SetRemoteDescription(offer); err != nil {
		log.Printf("GuidanceApp: Error setting remote description: %v", err)
		a.inviteTx.Respond(BuildResponse(488, "Not Acceptable Here", req, nil))
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("GuidanceApp: Error creating SDP answer: %v", err)
		a.inviteTx.Respond(BuildResponse(500, "Server Internal Error", req, nil))
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("GuidanceApp: Error setting local description: %v", err)
		a.inviteTx.Respond(BuildResponse(500, "Server Internal Error", req, nil))
		return
	}

	okRes := BuildResponse(200, "OK", req, nil)
	okRes.Headers["Content-Type"] = "application/sdp"
	okRes.Body = []byte(answer.SDP)
	if GetTag(okRes.Headers["To"]) == "" {
		okRes.Headers["To"] = okRes.Headers["To"] + ";tag=" + GenerateTag()
	}
	a.remoteTag = GetTag(okRes.Headers["To"])

	if err := a.inviteTx.Respond(okRes); err != nil {
		log.Printf("GuidanceApp: Error sending 200 OK: %v", err)
		return
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("GuidanceApp: PeerConnection State has changed: %s", state.String())
		if state == webrtc.PeerConnectionStateConnected {
			go a.streamWavFile()
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			select {
			case <-a.done:
			default:
				close(a.done)
			}
		}
	})

	log.Printf("GuidanceApp: Call answered for %s. Waiting for ACK.", a.callID)
	<-a.inviteTx.Done()
	log.Printf("GuidanceApp: ACK received or transaction timed out for %s.", a.callID)

	<-a.done
	log.Printf("GuidanceApp: Session finished for %s.", a.callID)
}

func (a *App) streamWavFile() {
	defer func() {
		log.Println("GuidanceApp: Finished streaming audio file.")
		a.sendBye()
		a.peerConnection.Close()
	}()

	file, err := os.Open(wavFilePath)
	if err != nil {
		log.Printf("GuidanceApp: Could not open wav file: %v", err)
		return
	}
	defer file.Close()

	wavReader := wav.NewDecoder(file)
	if !wavReader.IsValidFile() {
		log.Println("GuidanceApp: Invalid wav file")
		return
	}

	if wavReader.SampleRate != rtpSampleRate || wavReader.NumChans != 1 || wavReader.BitDepth != 16 {
		log.Printf("GuidanceApp: Unsupported WAV file format. Must be 16-bit %dHz mono.", rtpSampleRate)
		return
	}

	pcmBuf := &audio.IntBuffer{
		Data:   make([]int, 2048),
		Format: wavReader.Format(),
	}

	samplesPerPacket := rtpSampleRate * rtpPacketDuration / 1000
	ticker := time.NewTicker(rtpPacketDuration * time.Millisecond)
	defer ticker.Stop()

	log.Println("GuidanceApp: Starting to stream WAV file...")
	var seqNum uint16
	var timestamp uint32

	for ; ; <-ticker.C {
		n, err := wavReader.PCMBuffer(pcmBuf)
		if err == io.EOF {
			log.Println("GuidanceApp: Reached end of WAV file.")
			return
		}
		if err != nil {
			log.Printf("GuidanceApp: Error reading from wav file: %v", err)
			return
		}
		if n == 0 {
			continue
		}

		samples := pcmBuf.Data[:n]
		int16Samples := make([]int16, n)
		for i, s := range samples {
			int16Samples[i] = int16(s)
		}

		// Convert []int16 to []byte for G.711 encoding
		buf := new(bytes.Buffer)
		for _, s := range int16Samples {
			binary.Write(buf, binary.LittleEndian, s)
		}

		ulawSamples := g711.EncodeUlaw(buf.Bytes())

		if err := a.audioTrack.WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    0, // PCMU
				SequenceNumber: seqNum,
				Timestamp:      timestamp,
				SSRC:           uint32(a.ssrc),
			},
			Payload: ulawSamples,
		}); err != nil {
			log.Printf("GuidanceApp: Error writing RTP packet: %v", err)
			return
		}
		seqNum++
		timestamp += uint32(samplesPerPacket)
	}
}

func (a *App) sendBye() {
	req := a.inviteTx.OriginalRequest()
	nextCSeq := atomic.AddUint32(&a.cseq, 1)

	byeReq := &SIPRequest{
		Method: "BYE",
		URI:    a.remoteTarget,
		Proto:  "SIP/2.0",
		Headers: map[string]string{
			"Via":            fmt.Sprintf("SIP/2.0/%s %s;branch=%s", a.inviteTx.Transport().GetProto(), a.server.ListenAddr(), GenerateBranchID()),
			"From":           fmt.Sprintf("%s;tag=%s", req.GetHeader("To"), a.remoteTag),
			"To":             fmt.Sprintf("%s;tag=%s", req.GetHeader("From"), a.localTag),
			"Call-ID":        a.callID,
			"CSeq":           fmt.Sprintf("%d BYE", nextCSeq),
			"Max-Forwards":   "70",
			"Content-Length": "0",
		},
		Body: []byte{},
	}

	dest, err := ParseSIPURI(byeReq.URI)
	if err != nil {
		log.Printf("GuidanceApp: Failed to parse BYE request URI: %v", err)
		return
	}

	transport, err := a.server.TransportFor(dest.Host, dest.Port, a.inviteTx.Transport().GetProto())
	if err != nil {
		log.Printf("GuidanceApp: Failed to get transport for BYE: %v", err)
		return
	}

	tx, err := a.server.NewClientTx(byeReq, transport)
	if err != nil {
		log.Printf("GuidanceApp: Failed to create BYE transaction: %v", err)
		return
	}

	log.Printf("GuidanceApp: Sent BYE for call %s", a.callID)
	go func() {
		select {
		case res, ok := <-tx.Responses():
			if !ok {
				return
			}
			log.Printf("GuidanceApp: Received %d response for BYE", res.StatusCode)
		case <-time.After(10 * time.Second):
			log.Println("GuidanceApp: Timeout waiting for BYE response")
		}
	}()
}
