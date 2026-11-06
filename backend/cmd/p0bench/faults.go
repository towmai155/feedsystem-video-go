package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/video"
	amqp "github.com/rabbitmq/amqp091-go"
)

func require(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

func waitFor(check func() bool) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("reliability check timed out")
}

func (b *bench) faultChecks() {
	checks := map[string]any{"passed": false}
	b.report.Checks["reliability"] = checks
	name := b.project + "-rabbitmq-1"
	owner, err := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, name).Output()
	must(err)
	require(strings.TrimSpace(string(owner)) == b.project, "refusing to stop a container outside this test project")
	likeID, commentID := uint(videos+51), uint(videos+52)
	for _, id := range []uint{likeID, commentID} {
		must(b.db.Create(&video.Video{ID: id, AuthorID: users + 1, Username: "p0_author", Title: "reliability", PlayURL: "/static/f.mp4", CoverURL: "/static/f.jpg", CreateTime: time.Now()}).Error)
	}
	log.Printf("FAULT CHECK: stopping only %s", name)
	must(exec.Command("docker", "stop", "--time", "5", name).Run())
	stopped := true
	defer func() {
		if stopped {
			_ = exec.Command("docker", "start", name).Run()
		}
	}()
	like := b.run("mq_outage_like", 20, 0, 20, func(i int) observation { return b.write("like", likeID, i, i, "mq-outage-like") })
	comment := b.run("mq_outage_comment", 20, 0, 20, func(i int) observation { return b.write("comment", commentID, i, i, "mq-outage-comment") })
	b.report.Results = append(b.report.Results, like, comment)
	require(like.OK == 20 && comment.OK == 20, "writes failed while broker was unavailable")
	var pending int64
	must(b.db.Model(&events.OutboxEvent{}).Where("published_at IS NULL").Count(&pending).Error)
	checks["pending_events_while_mq_down"] = pending
	require(pending == 80, "business commits did not retain all 80 events")
	b.save()
	must(exec.Command("docker", "start", name).Run())
	stopped = false
	waitFor(func() bool { _, err := b.queues(); return err == nil })
	b.verifyWrite(like, "like", likeID, 20, "mq-outage-like", false)
	b.verifyWrite(comment, "comment", commentID, 20, "mq-outage-comment", false)
	require(like.Verification["passed"] == true && comment.Verification["passed"] == true, "broker recovery lost effects")
	waitFor(func() bool {
		var n int64
		err := b.db.Model(&events.OutboxEvent{}).Where("published_at IS NULL").Count(&n).Error
		return err == nil && n == 0
	})
	checks["mq_recovery_without_process_restart"] = true

	cfg := config.RabbitMQConfig{Host: "127.0.0.1", Port: 15673, Username: "p0", Password: "p0-local-only"}
	publisher := rabbitmq.NewConfirmedPublisher(cfg)
	defer publisher.Close()
	publish := func(exchange, key, id string, body []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		must(publisher.Publish(ctx, exchange, key, amqp.Publishing{MessageId: id, Body: body, DeliveryMode: amqp.Persistent, ContentType: "application/json", Timestamp: time.Now()}))
	}
	confirmCtx, cancelConfirm := context.WithTimeout(context.Background(), 5*time.Second)
	err = publisher.Publish(confirmCtx, "like.events", "probe.unroutable", amqp.Publishing{Body: []byte(`{}`), DeliveryMode: amqp.Persistent})
	cancelConfirm()
	require(err != nil, "unroutable publication was incorrectly reported successful")
	checks["unroutable_publish_rejected"] = true
	// Simulate confirmed publications whose local completion mark/ACK was lost.
	for _, key := range []string{"like.like", "video.popularity.update"} {
		var row events.OutboxEvent
		must(b.db.Where("routing_key = ? AND JSON_EXTRACT(payload, '$.video_id') = ?", key, likeID).First(&row).Error)
		for i := 0; i < 5; i++ {
			publish(row.Exchange, row.RoutingKey, row.ID, []byte(row.Payload))
		}
	}
	// A wrong Redis type reliably produces a real processing failure, while
	// leaving other test traffic untouched in its separate historical minute.
	window := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	windowKey := "v1:hot:video:1m:" + window.Format("200601021504")
	ctx := context.Background()
	n, err := b.rdb.Exists(ctx, windowKey).Result()
	must(err)
	require(n == 0, "refusing to overwrite an existing score window")
	must(b.rdb.Set(ctx, windowKey, "intentional-wrong-type", 0).Err())
	poisonID, err := events.NewID()
	must(err)
	evt := rabbitmq.PopularityEvent{EventID: poisonID, VideoID: videos + 53, Change: 7, OccurredAt: window.Add(10 * time.Second)}
	body, err := json.Marshal(evt)
	must(err)
	publish("video.popularity.events", "video.popularity.update", poisonID, body)
	mq, err := rabbitmq.NewRabbitMQ(&cfg)
	must(err)
	defer mq.Close()
	var quarantined amqp.Delivery
	waitFor(func() bool {
		d, ok, err := mq.Ch.Get("video.popularity.events.dlx", false)
		must(err)
		if !ok {
			return false
		}
		require(d.MessageId == poisonID, "unexpected message in fresh test DLQ")
		quarantined = d
		return true
	})
	n, err = b.rdb.Exists(ctx, "v1:processed:popularity:"+poisonID).Result()
	must(err)
	require(n == 0, "failed Redis operation was incorrectly marked processed")
	require(quarantined.Headers["x-original-queue"] == "video.popularity.events", "DLQ lost source queue metadata")
	checks["redis_failure_quarantined"] = true
	checks["retry_count"] = quarantined.Headers["x-retry-count"]
	b.save()
	// Repair only the key introduced by this check, then replay the unchanged ID.
	must(b.rdb.Del(ctx, windowKey).Err())
	for i := 0; i < 5; i++ {
		publish("video.popularity.events", "video.popularity.update", poisonID, quarantined.Body)
	}
	must(quarantined.Ack(false))
	waitFor(func() bool {
		score, err := b.rdb.ZScore(ctx, windowKey, fmt.Sprint(evt.VideoID)).Result()
		return err == nil && score == 7
	})
	waitFor(func() bool {
		queues, err := b.queues()
		if err != nil {
			return false
		}
		for _, q := range queues {
			if q.Ready+q.Unacked != 0 {
				return false
			}
		}
		return true
	})
	var notifications int64
	must(b.db.Table("notifications").Where("target_id = ? AND type = ?", likeID, "like").Count(&notifications).Error)
	require(notifications == 20, "duplicate message generated extra notifications")
	var score float64
	keys, err := b.rdb.Keys(ctx, "v1:hot:video:1m:*").Result()
	must(err)
	for _, key := range keys {
		value, err := b.rdb.ZScore(ctx, key, fmt.Sprint(likeID)).Result()
		if err == nil {
			score += value
		}
	}
	require(score == 20, "duplicate message incremented heat twice")
	checks["duplicate_notification_count"] = notifications
	checks["duplicate_heat_score"] = score
	checks["replayed_poison_score"] = 7
	checks["passed"] = true
	log.Printf("FAULT CHECKS PASSED: broker outage recovery, Redis quarantine/replay, event deduplication")
}
