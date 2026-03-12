// Package scheduler runs periodic scrape jobs using robfig/cron.
// Both weekly deal scrapes and daily price snapshots run in the same process —
// no external cron service needed.
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/wiekstras/supermarkt-scraper/db"
	"github.com/wiekstras/supermarkt-scraper/scraper"
)

type Scheduler struct {
	c      *cron.Cron
	stores []scraper.Store
}

func New(stores []scraper.Store) *Scheduler {
	return &Scheduler{
		c:      cron.New(cron.WithLocation(time.UTC)),
		stores: stores,
	}
}

// Start registers cron jobs and starts the scheduler in the background.
func (s *Scheduler) Start() {
	// Weekly deal scrape — every Monday at 06:00 UTC
	s.c.AddFunc("0 6 * * 1", func() {
		log.Println("[Scheduler] Wekelijkse deals scrape gestart")
		s.ScrapeDeals()
	})

	// Daily price snapshot — every day at 02:00 UTC
	s.c.AddFunc("0 2 * * *", func() {
		log.Println("[Scheduler] Dagelijkse prijssnapshot gestart")
		s.ScrapeDeals()
	})

	s.c.Start()
	log.Println("[Scheduler] Gestart: maandag 06:00 deals + dagelijks 02:00 snapshot")
}

func (s *Scheduler) Stop() {
	s.c.Stop()
}

// ScrapeDeals is also called directly from the API (POST /api/scrape/deals).
func (s *Scheduler) ScrapeDeals() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, store := range s.stores {
		go func(st scraper.Store) {
			start := time.Now()
			deals, err := st.HaalDealsOp(ctx)
			if err != nil {
				log.Printf("[Scheduler] %s deals fout: %v", st.Name(), err)
				return
			}
			log.Printf("[Scheduler] %s: %d deals opgehaald in %v", st.Name(), len(deals), time.Since(start))

			if err := db.SlaDealsOp(deals); err != nil {
				log.Printf("[Scheduler] %s DB deals fout: %v", st.Name(), err)
				return
			}
			if err := db.SlaaDealsPrijsSnapshotOp(deals); err != nil {
				log.Printf("[Scheduler] %s DB snapshot fout: %v", st.Name(), err)
			}
			log.Printf("[Scheduler] %s: %d deals opgeslagen", st.Name(), len(deals))
		}(store)
	}
}
