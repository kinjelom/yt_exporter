package youtrack

import (
	"bytes"
	"encoding/json"
)

// Value is a telemetry attribute that YouTrack documents as a string or a number. It accepts both JSON forms
// (and null), so a type change on the server side does not break decoding of the whole response.
type Value string

func (v *Value) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*v = ""
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*v = Value(s)
	default:
		*v = Value(b)
	}
	return nil
}

// AppConfig is the subset of /api/config used by the exporter.
type AppConfig struct {
	Version string `json:"version"`
	Build   string `json:"build"`
}

// Telemetry is /api/admin/telemetry (Administration > Server Settings > Global Settings > View server metrics).
type Telemetry struct {
	AvailableProcessors                Value        `json:"availableProcessors"`
	AvailableMemory                    Value        `json:"availableMemory"`
	AllocatedMemory                    Value        `json:"allocatedMemory"`
	UsedMemory                         Value        `json:"usedMemory"`
	StartedTime                        Value        `json:"startedTime"`
	DatabaseBackgroundThreads          Value        `json:"databaseBackgroundThreads"`
	PendingAsyncJobs                   Value        `json:"pendingAsyncJobs"`
	CachedResultsCountInDBQueriesCache Value        `json:"cachedResultsCountInDBQueriesCache"`
	DatabaseQueriesCacheHitRate        Value        `json:"databaseQueriesCacheHitRate"`
	BlobStringsCacheHitRate            Value        `json:"blobStringsCacheHitRate"`
	TotalTransactions                  Value        `json:"totalTransactions"`
	TransactionsPerSecond              Value        `json:"transactionsPerSecond"`
	RequestsPerSecond                  Value        `json:"requestsPerSecond"`
	DatabaseSize                       Value        `json:"databaseSize"`
	FullDatabaseSize                   Value        `json:"fullDatabaseSize"`
	TextIndexSize                      Value        `json:"textIndexSize"`
	OnlineUsers                        *OnlineUsers `json:"onlineUsers"`
	ReportCalculatorThreads            Value        `json:"reportCalculatorThreads"`
	NotificationAnalyzerThreads        Value        `json:"notificationAnalyzerThreads"`
}

type OnlineUsers struct {
	Users Value `json:"users"`
}

type BackupStatus struct {
	BackupInProgress bool         `json:"backupInProgress"`
	BackupCancelled  bool         `json:"backupCancelled"`
	BackupError      *BackupError `json:"backupError"`
}

type BackupError struct {
	Date         int64  `json:"date"`
	ErrorMessage string `json:"errorMessage"`
}

type BackupFile struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	CreationDate int64  `json:"creationDate"`
}

type License struct {
	Error string `json:"error"`
}

type Project struct {
	ID        string `json:"id"`
	ShortName string `json:"shortName"`
	Name      string `json:"name"`
	Archived  bool   `json:"archived"`
}

// ProjectCustomField is a custom field attached to a project. The bundle values are set for fields with a fixed set
// of values (enum, state, version, ...), not for fields that hold users, text, numbers or dates.
type ProjectCustomField struct {
	Field  CustomField `json:"field"`
	Bundle *Bundle     `json:"bundle"`
}

type CustomField struct {
	Name string `json:"name"`
}

type Bundle struct {
	Values []BundleValue `json:"values"`
}

type BundleValue struct {
	Name string `json:"name"`
}

// Issue holds the issue attributes the issue index reads. Updated and Resolved are Unix times in milliseconds,
// Resolved is nil while the issue is unresolved.
type Issue struct {
	ID           string             `json:"id"`
	Updated      int64              `json:"updated"`
	Resolved     *int64             `json:"resolved"`
	Project      *IssueProject      `json:"project"`
	CustomFields []IssueCustomField `json:"customFields"`
}

type IssueProject struct {
	ShortName string `json:"shortName"`
}

// IssueCustomField is the value of a custom field in an issue. The value is an object for a single-value field, an
// array for a multi-value field or null, it is decoded by ValueName.
type IssueCustomField struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

// ValueName returns the name of the field value, of the first value of a multi-value field, "" when it is empty.
func (f IssueCustomField) ValueName() string {
	var one struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(f.Value, &one) == nil {
		return one.Name
	}
	var many []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(f.Value, &many) == nil && len(many) > 0 {
		return many[0].Name
	}
	return ""
}

type User struct {
	ID     string `json:"id"`
	Login  string `json:"login"`
	Guest  bool   `json:"guest"`
	Banned bool   `json:"banned"`
	// UserType is available since YouTrack 2026.2: AGENT, STANDARD_USER or REPORTER.
	UserType *UserType `json:"userType"`
}

type UserType struct {
	ID string `json:"id"`
}
