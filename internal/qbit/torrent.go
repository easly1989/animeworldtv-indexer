package qbit

import "github.com/easly1989/animeworldtv-indexer/internal/downloader"

// torrent is one torrents/info entry, with the fields Sonarr and Radarr read.
type torrent struct {
	Hash             string  `json:"hash"`
	Name             string  `json:"name"`
	Size             int64   `json:"size"`
	TotalSize        int64   `json:"total_size"`
	Progress         float64 `json:"progress"`
	DLSpeed          int64   `json:"dlspeed"`
	UPSpeed          int64   `json:"upspeed"`
	ETA              int64   `json:"eta"`
	State            string  `json:"state"`
	Category         string  `json:"category"`
	Tags             string  `json:"tags"`
	SavePath         string  `json:"save_path"`
	ContentPath      string  `json:"content_path"`
	Downloaded       int64   `json:"downloaded"`
	AmountLeft       int64   `json:"amount_left"`
	Ratio            float64 `json:"ratio"`
	RatioLimit       float64 `json:"ratio_limit"`
	SeedingTimeLimit int64   `json:"seeding_time_limit"`
	InactiveLimit    int64   `json:"inactive_seeding_time_limit"`
	AddedOn          int64   `json:"added_on"`
	CompletionOn     int64   `json:"completion_on"`
	LastActivity     int64   `json:"last_activity"`
}

func toTorrent(j downloader.Job) torrent {
	t := torrent{
		Hash: j.Hash, Name: j.Name, Size: j.Size, TotalSize: j.Size, Category: j.Category,
		SavePath: j.SavePath, ContentPath: j.Path(), Downloaded: j.Done, AmountLeft: max(j.Size-j.Done, 0),
		// A ratio limit of 0 is "already reached": it lets Sonarr treat a
		// completed download as done seeding, so it moves the file and cleans up.
		RatioLimit: 0, SeedingTimeLimit: -2, InactiveLimit: -2,
		AddedOn: j.Added.Unix(), ETA: noETA, LastActivity: j.Added.Unix(),
	}
	switch j.State {
	case downloader.Completed:
		t.State, t.Progress, t.AmountLeft = "pausedUP", 1, 0
		t.Size, t.TotalSize, t.Downloaded = j.Done, j.Done, j.Done
		t.CompletionOn = j.Finished.Unix()
	case downloader.Failed:
		t.State = "error"
	default:
		t.State, t.DLSpeed = "downloading", j.Speed
		if j.Size > 0 {
			t.Progress = float64(j.Done) / float64(j.Size)
			// qBittorrent never shows 1 until the torrent is complete.
			t.Progress = min(t.Progress, 0.999)
		}
		if j.Speed > 0 && j.Size > 0 {
			t.ETA = t.AmountLeft / j.Speed
		}
	}
	return t
}
