package workflow

// Per-role node behaviour (notify send, validation rules) moved to
// graph_test.go. The http stage type was removed in the role-graph rework;
// URL fetching is now an analyst capability (`urls`).
