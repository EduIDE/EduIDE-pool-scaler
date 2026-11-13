package scaler

import (
	"math/rand/v2"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// ActiveStrategy picks the strategy for the day containing now, in loc:
//
//  1. a date pinned in switchback.schedule,
//  2. the configured active strategy when switchback is off,
//  3. switchback.examDayStrategy on a day with an exam,
//  4. otherwise a random rotation, balanced per weekday: each weekday runs
//     every strategy once in every block of len(strategies) weeks, in an
//     order drawn from the seed.
func ActiveStrategy(cfg config.Config, loc *time.Location, now time.Time, events []model.Event) string {
	local := now.In(loc)
	date := local.Format(time.DateOnly)
	if s, ok := cfg.Switchback.Schedule[date]; ok {
		return s
	}
	sb := cfg.Switchback
	if !sb.Enabled {
		return cfg.ActiveStrategy
	}
	if sb.ExamDayStrategy != "" {
		for _, e := range events {
			if e.Kind == model.EventExam && e.Time.In(loc).Format(time.DateOnly) == date {
				return sb.ExamDayStrategy
			}
		}
	}
	day := daysSinceEpoch(local)
	week, weekday := day/7, day%7
	n := int64(len(sb.Strategies))
	block := week / n
	r := rand.New(rand.NewPCG(uint64(sb.Seed), uint64(block*7+weekday)))
	return sb.Strategies[r.Perm(int(n))[week%n]]
}

// daysSinceEpoch counts calendar days, so a DST change cannot shift the date.
func daysSinceEpoch(local time.Time) int64 {
	y, m, d := local.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}
