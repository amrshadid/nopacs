package dir

import (
	"context"

	"github.com/amrshadid/nopacs/internal/blob"
)

var _ blob.Recaller = (*Store)(nil)

// Recall answers blob.RecallOnline for an object the directory holds: a
// file on a disk reads at once, so there is never anything to bring back,
// and a caller asks a recall without knowing the kind of storage. A key
// that holds nothing is NotFound, as on any store; a bad key or request is
// Invalid.
func (s *Store) Recall(ctx context.Context, key string, req blob.RecallRequest) (blob.Recall, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Recall{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Recall{}, err
	}
	if err := blob.CheckRecall(req); err != nil {
		return blob.Recall{}, err
	}
	if _, err := s.lstatObject(key); err != nil {
		return blob.Recall{}, err
	}
	return blob.Recall{State: blob.RecallOnline}, nil
}
