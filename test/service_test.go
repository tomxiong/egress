//go:build integration
// +build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	lksdk "github.com/livekit/server-sdk-go"
	"github.com/tomxiong/protocol/egress"
	"github.com/tomxiong/protocol/livekit"
	"github.com/tomxiong/protocol/utils"

	"github.com/livekit/egress/pkg/pipeline/params"
	"github.com/livekit/egress/pkg/service"
)

func testService(t *testing.T, conf *testConfig, room *lksdk.Room) {
	if room != nil {
		audioTrackID := publishSampleToRoom(t, room, params.MimeTypeOpus, false)
		t.Cleanup(func() { _ = room.LocalParticipant.UnpublishTrack(audioTrackID) })

		videoTrackID := publishSampleToRoom(t, room, params.MimeTypeVP8, conf.Muting)
		t.Cleanup(func() { _ = room.LocalParticipant.UnpublishTrack(videoTrackID) })
	}

	rc, err := getRedisClient(conf.Config)
	require.NoError(t, err)
	rpcServer := egress.NewRedisRPCServer(rc)
	rpcClient := egress.NewRedisRPCClient("egress_test", rc)

	// start service
	svc := service.NewService(conf.Config, rpcServer)
	go func() {
		err := svc.Run()
		require.NoError(t, err)
	}()
	t.Cleanup(func() { svc.Stop(true) })

	// subscribe to result channel
	sub, err := rpcClient.GetUpdateChannel(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	// startup time
	time.Sleep(time.Second * 2)

	// check status
	if conf.HealthPort != 0 {
		status := getStatus(t, svc)
		require.Len(t, status, 1)
		require.Contains(t, status, "CpuLoad")
	}

	// send start request
	filename := fmt.Sprintf("service-%v.mp4", time.Now().Unix())

	filepath := getFilePath(conf.Config, filename)
	info, err := rpcClient.SendRequest(context.Background(), &livekit.StartEgressRequest{
		RoomId: room.SID,
		WsUrl:  conf.WsUrl,
		Request: &livekit.StartEgressRequest_RoomComposite{
			RoomComposite: &livekit.RoomCompositeEgressRequest{
				RoomName: room.Name,
				Layout:   "speaker-dark",
				Output: &livekit.RoomCompositeEgressRequest_File{
					File: &livekit.EncodedFileOutput{
						Filepath: filepath,
					},
				},
			},
		},
	})

	// check returned egress info
	require.NoError(t, err)
	require.Empty(t, info.Error)
	require.NotEmpty(t, info.EgressId)
	require.Equal(t, room.SID, info.RoomId)
	require.Equal(t, livekit.EgressStatus_EGRESS_STARTING, info.Status)

	egressID := info.EgressId

	// check status
	if conf.HealthPort != 0 {
		status := getStatus(t, svc)
		require.Len(t, status, 2)
		require.Contains(t, status, egressID)
	}

	// wait
	time.Sleep(time.Second * 10)

	// check active update
	checkUpdate(t, sub, egressID, livekit.EgressStatus_EGRESS_ACTIVE)

	// wait
	time.Sleep(time.Second * 5)

	// send stop request
	info, err = rpcClient.SendRequest(context.Background(), &livekit.EgressRequest{
		EgressId: egressID,
		Request: &livekit.EgressRequest_Stop{
			Stop: &livekit.StopEgressRequest{
				EgressId: egressID,
			},
		},
	})

	// check egress info
	require.NoError(t, err)
	require.Empty(t, info.Error)
	require.NotEmpty(t, info.StartedAt)
	require.Equal(t, livekit.EgressStatus_EGRESS_ENDING, info.Status)

	// wait
	time.Sleep(time.Second * 2)

	// check ending update
	checkUpdate(t, sub, egressID, livekit.EgressStatus_EGRESS_ENDING)

	// wait
	time.Sleep(time.Second * 2)

	// check complete update
	info = checkUpdate(t, sub, egressID, livekit.EgressStatus_EGRESS_COMPLETE)

	// check status
	if conf.HealthPort != 0 {
		status := getStatus(t, svc)
		require.Len(t, status, 1)
	}

	// expected params
	p := &params.Params{
		AudioParams: params.AudioParams{
			AudioEnabled:   true,
			AudioCodec:     params.MimeTypeAAC,
			AudioBitrate:   128,
			AudioFrequency: 44100,
		},
		VideoParams: params.VideoParams{
			VideoEnabled: true,
			VideoCodec:   params.MimeTypeH264,
			VideoProfile: params.ProfileMain,
			Width:        1920,
			Height:       1080,
			Framerate:    30,
			VideoBitrate: 4500,
		},
		OutputType: params.OutputTypeMP4,
	}

	verifyFile(t, filepath, p, info, conf.Muting)
}

func getStatus(t *testing.T, svc *service.Service) map[string]interface{} {
	b, err := svc.Status()
	require.NoError(t, err)

	status := make(map[string]interface{})
	err = json.Unmarshal(b, &status)
	require.NoError(t, err)

	return status
}

func checkUpdate(t *testing.T, sub utils.PubSub, egressID string, status livekit.EgressStatus) *livekit.EgressInfo {
	select {
	case msg := <-sub.Channel():
		b := sub.Payload(msg)
		info := &livekit.EgressInfo{}
		require.NoError(t, proto.Unmarshal(b, info))
		require.Empty(t, info.Error)
		require.Equal(t, egressID, info.EgressId)
		require.Equal(t, status, info.Status)
		return info

	default:
		t.Fatal("no update from results channel")
		return nil
	}
}
