package cmd

import (
	"fmt"
	"path/filepath"
	"sync/atomic"

	"github.com/SurgeDM/Surge/internal/config"
	"github.com/SurgeDM/Surge/internal/store"
	"github.com/SurgeDM/Surge/internal/utils"
)

func runStartupIntegrityCheck() string {
	// Normalize downloads stuck in "downloading" status from a prior crash/kill.
	// This must happen before ValidateIntegrity so the newly-paused entries
	// are included in the integrity check and eligible for auto-resume.
	if normalized, err := store.NormalizeStaleDownloads(); err != nil {
		msg := fmt.Sprintf("Startup: failed to normalize stale downloads: %v", err)
		utils.Debug("%s", msg)
	} else if normalized > 0 {
		utils.Debug("Startup: normalized %d stale downloading entries to paused", normalized)
	}

	// Validate integrity of paused/queued downloads before auto-resume.
	// This removes entries whose .surge files are missing/tampered and
	// also cleans orphan .surge files that no longer have DB entries.
	if removed, err := store.ValidateIntegrity(); err != nil {
		msg := fmt.Sprintf("Startup integrity check failed: %v", err)
		return msg
	} else if removed > 0 {
		msg := fmt.Sprintf("Startup integrity check: removed %d corrupted/orphaned downloads", removed)
		return msg
	}
	utils.Debug("%s", "Startup integrity check: no issues found")
	return ""
}

// initializeGlobalState sets up the environment for a process that hosts the
// engine (the TUI and `surge server`): the store, and a debug log of its own.
func initializeGlobalState() error {
	if err := initializeClientState(); err != nil {
		return err
	}

	utils.ConfigureDebug(config.GetLogsDir())
	utils.Debug("Surge %s (commit %s)", Version, Commit)

	// Clean up old logs (keeping retention-1 because a new log will be created immediately after)
	retention := config.Resolve[int](getSettings().General.LogRetentionCount)
	if retention > 0 {
		utils.CleanupLogs(retention - 1)
	} else {
		utils.CleanupLogs(retention)
	}
	return nil
}

// initializeClientState is the subset a thin client command needs: the
// directories, the store and the settings. It deliberately does not open a
// debug log. A client such as `surge ls` runs once per poll from the desktop
// widget, and a log file per invocation would rotate the server's own log
// out of the retention window within seconds.
func initializeClientState() error {
	if err := config.EnsureDirs(); err != nil {
		return fmt.Errorf("failed to create surge directories: %w", err)
	}
	store.Configure(filepath.Join(config.GetStateDir(), "surge.db"))
	return nil
}

func getSettings() *config.Settings {
	if globalSettings != nil {
		return globalSettings
	}
	settings, err := config.LoadSettings()
	if err != nil {
		utils.Debug("Warning: failed to load settings: %v - using defaults", err)
		defaults := config.DefaultSettings()
		defaults.StartupWarnings = append(defaults.StartupWarnings,
			fmt.Sprintf("Config: failed to read settings file (%v) - all settings reset to defaults", err))
		return defaults
	}
	return settings
}

func resumePausedDownloads() {
	settings := getSettings()

	pausedEntries, err := store.LoadPausedDownloads()
	if err != nil {
		return
	}

	for _, entry := range pausedEntries {
		// If entry is explicitly queued, we should start it regardless of AutoResume setting
		// If entry is paused, we only start it if AutoResume is enabled
		if entry.Status == "paused" && !config.Resolve[bool](settings.General.AutoResume) {
			continue
		}
		if GlobalService == nil || entry.ID == "" {
			continue
		}
		if err := GlobalService.Resume(entry.ID); err == nil {
			atomic.AddInt32(&activeDownloads, 1)
		}
	}
}
