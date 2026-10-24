package db

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCleanupArtifactPublicationPreservesConcurrentConsumerDeclaration(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	if err := store.CreateJob(ctx, Job{ID: "producer", Agent: "lead", Type: "ask", State: "succeeded", Payload: `{"result":{"decision":"approved"}}`}); err != nil {
		t.Fatal(err)
	}
	collected, err := store.GetJob(ctx, "producer")
	if err != nil {
		t.Fatal(err)
	}
	const consumers = `{"decision":"approved","delegations":[{"id":"late-consumer","depends_on":["producer"]}]}`
	const changed = `{"result":` + consumers + `}`
	if err := store.UpdateJobPayload(ctx, collected.ID, changed); err != nil {
		t.Fatal(err)
	}
	const snapshot = `{"resource_id":"owned-resource","lifecycle_generation":0}`
	if written, err := store.RecordCleanupArtifactSnapshot(ctx, collected, snapshot); err != nil || written {
		t.Fatalf("stale publication overwrote a consumer declaration: written=%t err=%v", written, err)
	}
	current, err := store.GetJob(ctx, collected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Payload != changed || current.LifecycleGeneration != collected.LifecycleGeneration {
		t.Fatalf("same-generation consumer update was lost: %+v", current)
	}
	if written, err := store.RecordCleanupArtifactSnapshot(ctx, current, snapshot); err != nil || !written {
		t.Fatalf("current snapshot publication: written=%t err=%v", written, err)
	}
	current, err = store.GetJob(ctx, collected.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(current.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["result"]) != consumers || string(payload["cleanup_artifacts"]) != snapshot {
		t.Fatalf("publication did not preserve consumer dependencies alongside its evidence: %s", current.Payload)
	}
}
