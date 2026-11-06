package social

import (
	"context"
	"testing"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/testutil"

	"gorm.io/gorm"
)

func seedAccounts(t *testing.T, db *gorm.DB) (follower, vlogger *account.Account) {
	t.Helper()
	follower = &account.Account{Username: "follower"}
	vlogger = &account.Account{Username: "vlogger"}
	if err := db.Create(follower).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(vlogger).Error; err != nil {
		t.Fatal(err)
	}
	return follower, vlogger
}

func TestFollowCommitsRelationAndEventTogether(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&Social{}, &account.Account{}, &events.OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	follower, vlogger := seedAccounts(t, db)
	s := NewSocialService(NewSocialRepository(db), account.NewAccountRepository(db))

	if err := s.Follow(context.Background(), &Social{FollowerID: follower.ID, VloggerID: vlogger.ID}); err != nil {
		t.Fatal(err)
	}
	var relations int64
	db.Model(&Social{}).Count(&relations)
	if relations != 1 {
		t.Fatalf("expected one follow relation, got %d", relations)
	}
	var pending []events.OutboxEvent
	if err := db.Find(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected one durable follow event, got %d", len(pending))
	}
	if pending[0].Exchange != "social.events" || pending[0].RoutingKey != "social.follow" {
		t.Fatalf("unexpected event route: %s/%s", pending[0].Exchange, pending[0].RoutingKey)
	}

	// A duplicate request must not append another event.
	if err := s.Follow(context.Background(), &Social{FollowerID: follower.ID, VloggerID: vlogger.ID}); err == nil {
		t.Fatal("duplicate follow should be rejected")
	}
	db.Model(&events.OutboxEvent{}).Count(&relations)
	if relations != 1 {
		t.Fatalf("duplicate follow created another event: %d", relations)
	}
}

func TestFollowRollsBackWhenEventCannotBeStored(t *testing.T) {
	db := testutil.NewMySQL(t)
	// The outbox table is deliberately missing to force the event write to fail.
	if err := db.AutoMigrate(&Social{}, &account.Account{}); err != nil {
		t.Fatal(err)
	}
	follower, vlogger := seedAccounts(t, db)
	s := NewSocialService(NewSocialRepository(db), account.NewAccountRepository(db))

	if err := s.Follow(context.Background(), &Social{FollowerID: follower.ID, VloggerID: vlogger.ID}); err == nil {
		t.Fatal("missing event table should fail the transaction")
	}
	var relations int64
	db.Model(&Social{}).Count(&relations)
	if relations != 0 {
		t.Fatalf("follow row survived a rolled back event write: %d", relations)
	}
}

func TestUnfollowCommitsRemovalAndEventTogether(t *testing.T) {
	db := testutil.NewMySQL(t)
	if err := db.AutoMigrate(&Social{}, &account.Account{}, &events.OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	follower, vlogger := seedAccounts(t, db)
	s := NewSocialService(NewSocialRepository(db), account.NewAccountRepository(db))
	social := &Social{FollowerID: follower.ID, VloggerID: vlogger.ID}
	if err := s.Follow(context.Background(), social); err != nil {
		t.Fatal(err)
	}
	if err := s.Unfollow(context.Background(), social); err != nil {
		t.Fatal(err)
	}
	var relations int64
	db.Model(&Social{}).Count(&relations)
	if relations != 0 {
		t.Fatalf("follow relation still present: %d", relations)
	}
	var unfollows int64
	db.Model(&events.OutboxEvent{}).Where("routing_key = ?", "social.unfollow").Count(&unfollows)
	if unfollows != 1 {
		t.Fatalf("expected one unfollow event, got %d", unfollows)
	}
}
