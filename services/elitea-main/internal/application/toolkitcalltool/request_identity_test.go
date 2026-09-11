package toolkitcalltool

import (
	"strconv"
	"testing"
)

func TestToolRunIdentitySeparatesNewActionsFromRetries(t *testing.T) {
	next := 0
	service := &RunService{newID: func() (string, error) {
		next++
		return "generated-" + strconv.Itoa(next), nil
	}}
	request := validRequest()
	inputs := testInputs()
	first, err := service.idempotencyKey(request, inputs)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := service.idempotencyKey(request, inputs)
	if err != nil || retry != first {
		t.Fatalf("retry identity changed: %v", err)
	}
	request.RequestID = "next-click"
	second, err := service.idempotencyKey(request, inputs)
	if err != nil || second == first {
		t.Fatalf("new click reused previous run: %v", err)
	}
	request.RequestID = ""
	legacyFirst, err := service.idempotencyKey(request, inputs)
	if err != nil {
		t.Fatal(err)
	}
	legacySecond, err := service.idempotencyKey(request, inputs)
	if err != nil || legacyFirst == legacySecond {
		t.Fatalf("requests without identity reused a run: %v", err)
	}
}
