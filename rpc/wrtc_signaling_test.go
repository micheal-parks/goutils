package rpc

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/edaniels/golog"
	"github.com/pion/webrtc/v3"
	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	echopb "go.viam.com/utils/proto/rpc/examples/echo/v1"
	webrtcpb "go.viam.com/utils/proto/rpc/webrtc/v1"
	echoserver "go.viam.com/utils/rpc/examples/echo/server"
	"go.viam.com/utils/testutils"
)

func TestWebRTCSignalingWithMemoryQueue(t *testing.T) {
	testutils.SkipUnlessInternet(t)
	logger := golog.NewTestLogger(t)
	signalingCallQueue := NewMemoryWebRTCCallQueue(logger)
	testWebRTCSignaling(t, signalingCallQueue, logger)
	test.That(t, signalingCallQueue.Close(), test.ShouldBeNil)
}

func TestWebRTCSignalingWithMongoDBQueue(t *testing.T) {
	testutils.SkipUnlessInternet(t)
	logger := golog.NewTestLogger(t)
	client := testutils.BackingMongoDBClient(t)
	signalingCallQueue, err := NewMongoDBWebRTCCallQueue(client, logger)
	test.That(t, err, test.ShouldBeNil)
	testWebRTCSignaling(t, signalingCallQueue, logger)
	test.That(t, signalingCallQueue.Close(), test.ShouldBeNil)
}

//nolint:thelper
func testWebRTCSignaling(t *testing.T, signalingCallQueue WebRTCCallQueue, logger golog.Logger) {
	signalingServer := NewWebRTCSignalingServer(signalingCallQueue, nil, logger)
	defer signalingServer.Close()

	grpcListener, err := net.Listen("tcp", "localhost:0")
	test.That(t, err, test.ShouldBeNil)
	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&webrtcpb.SignalingService_ServiceDesc, signalingServer)

	serveDone := make(chan error)
	go func() {
		serveDone <- grpcServer.Serve(grpcListener)
	}()

	webrtcServer := newWebRTCServer(logger)
	webrtcServer.RegisterService(&echopb.EchoService_ServiceDesc, &echoserver.Server{})

	hosts := []string{"yeehaw", "woahthere"}
	answerer := newWebRTCSignalingAnswerer(
		grpcListener.Addr().String(),
		hosts,
		webrtcServer,
		[]DialOption{WithInsecure()},
		webrtc.Configuration{},
		logger,
	)
	answerer.Start()

	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			cc, err := grpc.Dial(grpcListener.Addr().String(), grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			test.That(t, err, test.ShouldBeNil)
			defer func() {
				test.That(t, cc.Close(), test.ShouldBeNil)
			}()
			signalClient := webrtcpb.NewSignalingServiceClient(cc)

			callClient, err := signalClient.Call(context.Background(), &webrtcpb.CallRequest{})
			test.That(t, err, test.ShouldBeNil)
			_, err = callClient.Recv()
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, "expected rpc-host")

			md := metadata.New(map[string]string{"rpc-host": ""})
			callCtx := metadata.NewOutgoingContext(context.Background(), md)
			callClient, err = signalClient.Call(callCtx, &webrtcpb.CallRequest{})
			test.That(t, err, test.ShouldBeNil)
			_, err = callClient.Recv()
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, "non-empty rpc-host")

			md = metadata.New(map[string]string{"rpc-host": "one"})
			md.Append("rpc-host", "two")
			callCtx = metadata.NewOutgoingContext(context.Background(), md)
			callClient, err = signalClient.Call(callCtx, &webrtcpb.CallRequest{})
			test.That(t, err, test.ShouldBeNil)
			_, err = callClient.Recv()
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, "expected 1 rpc-host")

			md = metadata.New(map[string]string{"rpc-host": host})
			callCtx = metadata.NewOutgoingContext(context.Background(), md)

			callClient, err = signalClient.Call(callCtx, &webrtcpb.CallRequest{})
			test.That(t, err, test.ShouldBeNil)
			_, err = callClient.Recv()
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, "unexpected")

			callClient, err = signalClient.Call(callCtx, &webrtcpb.CallRequest{Sdp: "thing"})
			test.That(t, err, test.ShouldBeNil)
			_, err = callClient.Recv()
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, "illegal")

			for _, tc := range []bool{true, false} {
				t.Run(fmt.Sprintf("with trickle disabled %t", tc), func(t *testing.T) {
					ch, err := DialWebRTC(
						context.Background(),
						grpcListener.Addr().String(),
						host,
						logger,
						WithWebRTCOptions(DialWebRTCOptions{
							SignalingInsecure: true,
							DisableTrickleICE: tc,
						}),
					)
					test.That(t, err, test.ShouldBeNil)
					defer func() {
						test.That(t, ch.Close(), test.ShouldBeNil)
					}()

					echoClient := echopb.NewEchoServiceClient(ch)
					resp, err := echoClient.Echo(context.Background(), &echopb.EchoRequest{Message: "hello"})
					test.That(t, err, test.ShouldBeNil)
					test.That(t, resp.Message, test.ShouldEqual, "hello")
				})
			}
		})
	}

	webrtcServer.Stop()
	answerer.Stop()
	grpcServer.Stop()
	test.That(t, <-serveDone, test.ShouldBeNil)
}
