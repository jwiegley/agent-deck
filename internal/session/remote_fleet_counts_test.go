package session

import "testing"

// TestCountRemoteFleetBucketsEveryStatus: every session status a remote can
// report lands in exactly one RemoteFleetCounts bucket, so the buckets add up
// to Sessions. Status probes keep a queued session queued instead of calling
// its absent pane an error, so a remote now reports queued sessions, which
// countRemoteFleet used to count in Sessions and in no bucket.
func TestCountRemoteFleetBucketsEveryStatus(t *testing.T) {
	statuses := []Status{
		StatusRunning, StatusWaiting, StatusIdle, StatusError,
		StatusStarting, StatusStopped, StatusQueued,
	}
	sessions := make([]RemoteSessionInfo, 0, len(statuses))
	for _, status := range statuses {
		sessions = append(sessions, RemoteSessionInfo{ID: string(status), Status: string(status)})
	}
	counts := countRemoteFleet([]RemoteFleetRemote{
		{Name: "online", Online: true, Sessions: sessions},
		{Name: "offline", Sessions: sessions[len(sessions)-1:]},
	})

	want := RemoteFleetCounts{
		RemotesOnline: 1, RemotesOffline: 1, Sessions: len(statuses) + 1,
		Running: 2, Waiting: 1, Idle: 1, Error: 1, Stopped: 1, Queued: 2,
	}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if sum := counts.Running + counts.Waiting + counts.Idle + counts.Error + counts.Stopped + counts.Queued; sum != counts.Sessions {
		t.Fatalf("buckets sum to %d, want Sessions %d: %+v", sum, counts.Sessions, counts)
	}
}
