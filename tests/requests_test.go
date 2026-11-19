package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const requestID = "20261119T075914-25fd7a"

func request(from, to string) map[string]any {
	return map[string]any{"id": requestID, "from": from, "to": to, "status": "accepted", "text": "Help"}
}

func TestRemoteInboxRejectsOtherOperations(t *testing.T) {
	f := newFixture(t, "hub")
	for _, command := range []string{"", "bash", "deliver admin", "fetch; id", "ls", "--launch ls"} {
		f.fails(f.with("SSH_ORIGINAL_COMMAND="+command).internal("--remote-inbox"), "only deliver or fetch")
	}
	// The remote inbox runs only on the hub.
	outpost := newFixture(t, "outpost")
	outpost.fails(outpost.with("SSH_ORIGINAL_COMMAND=fetch").input("{}").internal("--remote-inbox"), "only deliver or fetch")
}

func TestMainEndpointRunsOnlyThreeExactCommands(t *testing.T) {
	f := newFixture(t, "outpost")
	for _, command := range []string{"", "bash", "agent ls; id", "agent ls ", "chezmoi apply", "agent restart admin"} {
		f.fails(f.with("SSH_ORIGINAL_COMMAND="+command).internal("--main-endpoint"), "main endpoint accepts only")
	}
	if calls := f.calls("chezmoi"); len(calls) != 0 {
		t.Fatalf("chezmoi ran for a refused command: %v", calls)
	}
	listing := f.ok(f.with("SSH_ORIGINAL_COMMAND=agent ls").internal("--main-endpoint"))
	if !strings.Contains(listing.stdout, "field lifecycle=active") {
		t.Fatalf("agent ls through the endpoint\n%s", listing)
	}
	restart := f.ok(f.with("SSH_ORIGINAL_COMMAND=agent restart all").internal("--main-endpoint"))
	if !strings.Contains(restart.stdout, "restart requested") {
		t.Fatalf("agent restart all through the endpoint\n%s", restart)
	}
	f.ok(f.with("SSH_ORIGINAL_COMMAND=chezmoi update").internal("--main-endpoint"))
	calls := f.calls("chezmoi")
	if len(calls) != 1 || strings.Join(calls[0].Args, " ") != "--no-tty update" || !calls[0].StdinDevNull {
		t.Fatalf("want one chezmoi --no-tty update with stdin from /dev/null, got %+v", calls)
	}
}

func TestHubQueuesRepliesForRemoteProject(t *testing.T) {
	f := newFixture(t, "hub")
	for _, project := range []string{"admin", "garden"} {
		f.open(project)
		f.write(filepath.Join(f.workspace(project), ".agent/inbox", requestID+".json"), mustJSON(t, request("field", project)))
		f.ok(f.with("PI_PROJECT="+project).agent("reply", project, requestID, "Verified"))
	}
	if calls := f.calls("ssh"); len(calls) != 0 {
		t.Fatalf("the hub never connects out, got %v", calls)
	}
	items := jsonFiles(t, f.path(".local/state/agents/remote-inbox/field"))
	senders := map[any]bool{}
	for _, item := range items {
		senders[item["from"]] = true
		if item["to"] != "field" || item["inReplyTo"] != requestID || item["text"] != "Verified" {
			t.Fatalf("queued reply %v", item)
		}
	}
	if len(items) != 2 || !senders["admin"] || !senders["garden"] {
		t.Fatalf("want replies from admin and garden, got %v", items)
	}
	fetch := f.with("SSH_ORIGINAL_COMMAND=fetch")
	var fetched []map[string]any
	must(t, json.Unmarshal([]byte(f.ok(fetch.input("{}").internal("--remote-inbox")).stdout), &fetched))
	if len(fetched) != 2 {
		t.Fatalf("fetch returned %v", fetched)
	}
	// An acknowledged reply leaves the queue.
	ack := mustJSON(t, map[string]any{"ack": []any{fetched[0]["id"]}})
	must(t, json.Unmarshal([]byte(f.ok(fetch.input(ack).internal("--remote-inbox")).stdout), &fetched))
	if len(fetched) != 1 {
		t.Fatalf("fetch after one acknowledgement returned %v", fetched)
	}
	f.fails(fetch.input(`{"ack": ["../escape"]}`).internal("--remote-inbox"), "acknowledgements")
}

func TestRemoteInboxValidatesDelivery(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	item := map[string]any{"id": requestID, "from": "field", "to": "garden", "text": "Help", "status": "new"}
	deliver := f.with("SSH_ORIGINAL_COMMAND=deliver")
	for _, change := range []map[string]any{
		{"from": "admin"}, {"to": "../escape"}, {"to": "field"}, {"to": "nobody"}, {"id": "../escape"},
		{"status": "accepted"}, {"text": " "}, {"text": strings.Repeat("x", 32768)},
	} {
		changed := map[string]any{}
		for key, value := range item {
			changed[key] = value
		}
		for key, value := range change {
			changed[key] = value
		}
		if r := deliver.input(mustJSON(t, changed)).internal("--remote-inbox"); r.code == 0 {
			t.Fatalf("delivery with %v accepted\n%s", change, r)
		}
	}
	deliver.fails(deliver.input("[]").internal("--remote-inbox"), "object")
	record := f.path(".local/state/agents/initialized/garden")
	original := f.read(record)
	initializing := readJSON[map[string]any](t, record)
	initializing["state"] = "initializing"
	f.write(record, mustJSON(t, initializing))
	deliver.fails(deliver.input(mustJSON(t, item)).internal("--remote-inbox"), "not initialized")
	f.write(record, original)
	out := f.ok(deliver.input(mustJSON(t, item)).internal("--remote-inbox"))
	if !strings.Contains(out.stdout, requestID) {
		t.Fatalf("delivery output\n%s", out)
	}
	stored := readJSON[map[string]any](t, filepath.Join(f.workspace("garden"), ".agent/inbox", requestID+".json"))
	if stored["from"] != "field" || stored["text"] != "Help" || stored["status"] != "new" {
		t.Fatalf("stored request %v", stored)
	}
	// A retried delivery is accepted, a different request under the same id is not.
	f.ok(deliver.input(mustJSON(t, item)).internal("--remote-inbox"))
	item["text"] = "Something else"
	deliver.fails(deliver.input(mustJSON(t, item)).internal("--remote-inbox"), "already exists")
}

func TestRemoteInboxFetchRefusesSymlinks(t *testing.T) {
	f := newFixture(t, "hub")
	queue := f.path(".local/state/agents/remote-inbox/field")
	must(t, os.MkdirAll(queue, 0o755))
	f.write("private.json", `{"secret": "not a reply"}`)
	link := filepath.Join(queue, requestID+".json")
	must(t, os.Symlink(f.path("private.json"), link))
	fetch := f.with("SSH_ORIGINAL_COMMAND=fetch").input("{}")
	f.fails(fetch.internal("--remote-inbox"), "queue entry")
	must(t, os.Remove(link))
	must(t, os.Remove(queue))
	must(t, os.Symlink(f.home, queue))
	f.fails(fetch.internal("--remote-inbox"), "Unsafe")
}

func TestOperatorRequestsAndRepliesUseTheCoordinatorInbox(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("admin")
	garden := f.open("garden")
	f.ok(f.operatorAsk("garden", "Water the plants"))
	requests := jsonFiles(t, filepath.Join(garden, ".agent/inbox"))
	if len(requests) != 1 || requests[0]["from"] != "alice" || requests[0]["to"] != "garden" {
		t.Fatalf("operator request %v", requests)
	}
	f.fails(f.with("PI_PROJECT=admin").operatorAsk("garden", "Pretend"), "cannot send as alice")
	f.fails(f.with("PI_PROJECT=alice").agent("ask", "garden", "Pretend"), "")
	// Replies to the operator and to a renamed sender land in the coordinator's inbox.
	for _, sender := range []string{"alice", "legacy"} {
		id := map[string]string{"alice": "20261119T075914-aaaaaa", "legacy": "20261119T075914-bbbbbb"}[sender]
		item := request(sender, "garden")
		item["id"] = id
		f.write(filepath.Join(garden, ".agent/inbox", id+".json"), mustJSON(t, item))
		f.ok(f.with("PI_PROJECT=garden").agent("reply", "garden", id, "Done"))
	}
	replies := jsonFiles(t, filepath.Join(f.workspace("admin"), ".agent/inbox"))
	if len(replies) != 2 {
		t.Fatalf("coordinator inbox %v", replies)
	}
	for _, reply := range replies {
		if reply["from"] != "garden" || reply["to"] != "admin" || reply["text"] != "Done" {
			t.Fatalf("reply %v", reply)
		}
	}
	f.fails(f.with("PI_PROJECT=admin").agent("reply", "garden", requestID, "Not mine"), "")
	f.fails(f.with("PI_PROJECT=garden").agent("ask", "garden", "Myself"), "own project")
}

func TestRemoteProjectDeliversAndFetchesOverSSH(t *testing.T) {
	f := newFixture(t, "outpost")
	f.write(filepath.Join(f.dir, "ssh-out.json"), `{"delivered": "x"}`)
	f.ok(f.with("PI_PROJECT=field").agent("ask", "garden", "Please check"))
	calls := f.calls("ssh")
	if len(calls) != 1 {
		t.Fatalf("ssh calls %v", calls)
	}
	args := calls[0].Args
	if flag(args, "-i") != f.path(".ssh/field-inbox") || args[len(args)-2] != "hub" || args[len(args)-1] != "deliver" {
		t.Fatalf("ssh arguments %q", args)
	}
	var sent map[string]any
	must(t, json.Unmarshal([]byte(calls[0].Stdin), &sent))
	if sent["from"] != "field" || sent["to"] != "garden" || sent["text"] != "Please check" || sent["status"] != "new" {
		t.Fatalf("delivered %v", sent)
	}
	f.fails(f.with("PI_PROJECT=admin").agent("ask", "garden", "Not allowed"), "Only field")

	reply := map[string]any{"id": "20261119T080000-cccccc", "from": "garden", "to": "field", "text": "Checked",
		"status": "new", "inReplyTo": requestID}
	f.write(filepath.Join(f.dir, "ssh-out.json"), mustJSON(t, []any{reply}))
	f.fails(f.with("PI_PROJECT=other").internal("--fetch-replies"), "fetch replies")
	f.ok(f.with("PI_PROJECT=field").internal("--fetch-replies"))
	stored := readJSON[map[string]any](t, filepath.Join(f.workspace("field"), ".agent/inbox", reply["id"].(string)+".json"))
	if stored["text"] != "Checked" {
		t.Fatalf("fetched reply %v", stored)
	}
	f.write(filepath.Join(f.dir, "ssh-out.json"), "[]")
	f.ok(f.with("PI_PROJECT=field").internal("--fetch-replies"))
	calls = f.calls("ssh")
	var acknowledged map[string][]string
	must(t, json.Unmarshal([]byte(calls[len(calls)-1].Stdin), &acknowledged))
	if !contains(acknowledged["ack"], reply["id"].(string)) || calls[len(calls)-1].Args[len(calls[len(calls)-1].Args)-1] != "fetch" {
		t.Fatalf("second fetch did not acknowledge %v: %+v", reply["id"], calls[len(calls)-1])
	}
}
