package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"strings"
	"testing"
)

func TestNodeRecoveryResultReadUsesExactAuthorizedOwnerProof(t *testing.T) {
	r, _, _ := recoveryFixture(t)
	r.ReplaySafety = domain.ReplaySafety{Kind: "completed_external_effect", ReceiptID: strings.Repeat("3", 64)}
	r.StopReason = "effect_reconciliation_required"
	r.AllowedActions = []string{"resume_result"}
	raw, _ := json.Marshal(r)
	raw, _ = domain.CanonicalReceipt(raw)
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	proof, data := recoveryEffectProof(r, "committed_result")
	resultSHA := sha256.Sum256(data)
	version := hex.EncodeToString(resultSHA[:])
	action, _ := json.Marshal(recoveryAction{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, LastAttempt: 1, Action: "resume_result", ReceiptSHA256: sha, OwnerProof: proof})
	for _, tc := range []struct {
		name                   string
		version, actor, status string
		data                   []byte
		deny                   bool
	}{{name: "authorized exact bytes", data: data}, {name: "foreign result version", version: strings.Repeat("9", 64), deny: true}, {name: "other actor", actor: "22", deny: true}, {name: "tampered wire", data: []byte("tampered"), deny: true}, {name: "not suspended", status: "RESUMED", deny: true}} {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.version
			if v == "" {
				v = version
			}
			actor := tc.actor
			if actor == "" {
				actor = "11"
			}
			status := tc.status
			if status == "" {
				status = "AUTHORIZED"
			}
			rows := append(recoveryClaimRows(), scriptedRow{values: []any{r.ActivationID, r.JournalRevision}}, scriptedRow{values: []any{raw, digest[:], action, actor, status}})
			e := &scriptedExecutor{rowResults: rows}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := recoveryRepo(t, s)
			owner := &recoveryProofStub{proof: proof, data: tc.data}
			repo.effects = owner
			got, err := repo.ReadNodeRecoveryResult(t.Context(), recoveryClaim(), strings.Repeat("3", 64), v)
			if tc.deny {
				if !errors.Is(err, storage.ErrContentRejected) || s.committed {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || string(got) != string(data) || !s.committed || len(e.execCalls) != 0 || owner.calls != 1 {
				t.Fatal(string(got), err)
			}
			got[0] = '!'
			if string(owner.data) != string(data) {
				t.Fatal("borrowed bytes escaped")
			}
		})
	}
}
