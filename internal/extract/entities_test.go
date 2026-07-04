package extract

import (
	"slices"
	"testing"
)

func TestEntitiesBasics(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"mid-sentence proper noun",
			"We decided to use Postgres instead of SQLite for the vector store.",
			[]string{"postgres", "sqlite"}},
		{"multi-word span",
			"The freeze starts the week before Black Friday according to Matt Patterson.",
			[]string{"black friday", "matt patterson"}},
		{"possessive normalized",
			"She has been reading Charlotte's Web to the kids.",
			[]string{"charlotte web"}},
		{"sentence-initial suppressed without evidence",
			"Previously, this foundation used paper records for everything they tracked.",
			nil},
		{"sentence-initial kept with mid-sentence evidence",
			"Sweden was cold. She moved away from Sweden four years ago.",
			[]string{"sweden"}},
		{"casing signal keeps initial token",
			"LGBTQ support groups meet weekly here.",
			[]string{"lgbtq"}},
		{"bracketed scaffolding skipped",
			"[8:00 pm on 8 May, 2023] Caroline: I went to a support group yesterday.",
			nil},
		{"speaker mention is an entity",
			"[8:00 pm on 8 May, 2023] Melanie: Hey Caroline! Good to see you!",
			[]string{"caroline"}},
		{"bare month is not an entity",
			"Someone mentioned that Maria took the last two weeks of December off.",
			[]string{"maria"}},
		{"quoted title",
			`We also watch "Elf" during the holidays with the kids.`,
			[]string{"elf"}},
		{"label line suppressed, decision subject kept",
			"Decision: for the database engine, the team standardized on Postgres with pgvector.",
			[]string{"postgres"}},
		{"numbers are not entities",
			"The dump ran 90 minutes over and finished at 4:30 in the morning.",
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Entities(tc.text)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Entities(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestEntitiesCap(t *testing.T) {
	text := "Alpha met Bravo. Then Charlie met Delta. Echo met Foxtrot. Golf met Hotel. " +
		"India met Juliett. Kilo met Lima. Mike met November2. Oscar met Papa. Quebec met Romeo."
	if got := Entities(text); len(got) > MaxEntities {
		t.Errorf("got %d entities, want <= %d: %v", len(got), MaxEntities, got)
	}
}

// entityGoldCase is one hand-labeled text: gold is the named entities a human
// reads in the message body (speaker labels and date scaffolding excluded —
// they are ingest framing, not content). Labeled before the extractor ran.
type entityGoldCase struct {
	text string
	gold []string
}

// locomoGold is a 30-turn random sample (seed 42) of LoCoMo dialogue turns in
// the exact shape the bench ingests ("[date] Speaker: text"), hand-labeled.
var locomoGold = []entityGoldCase{
	{"[1:32 pm on 6 January, 2024] Sam: Congrats on the news, Evan! You two look so happy in the pic. These moments make life so wonderful; super stoked for you!",
		[]string{"evan"}},
	{"[2:33 pm on 5 February, 2023] John: Thanks, Maria. Friendship means a lot to me. I'm glad we have each other's backs and can work towards a shared goal.",
		[]string{"maria"}},
	{"[8:56 pm on 20 July, 2023] Melanie: I'll always remember our camping trip last year when we saw the Perseid meteor shower. It was so amazing lying there and watching the sky light up with streaks of light. We all made wishes and felt so at one with the universe. That's a memory I'll never forget.",
		[]string{"perseid"}},
	{`[4:29 pm on 21 August, 2023] Tim: We also watch "Elf" during the holidays. It makes us laugh and get us feeling festive!`,
		[]string{"elf"}},
	{"[8:10 pm on 7 November, 2022] Nate: Even if it happens to a few, I'm sure at leasts one will make it to the screens and be your 3rd published movie!",
		nil},
	{"[10:57 am on 22 August, 2022] Nate: Enjoy it! Have a good day.",
		nil},
	{"[11:51 am on 3 June, 2023] Maria: That's a great lesson to pass on to your kids, John. Both are really important for strong relationships. Any plans to give another pet a loving home?",
		[]string{"john"}},
	{"[8:30 pm on 1 January, 2023] Maria: That's great, John! Empowering individuals through education and mentorship is crucial for helping them reach their goals. Can't wait to see the initiatives you come up with!",
		[]string{"john"}},
	{"[5:22 pm on 11 August, 2023] Dave: Thanks, Calvin! Yeah, I wanted a modern vibe but also that classic muscle car style. Really happy with it!",
		[]string{"calvin"}},
	{"[7:37 pm on 9 July, 2023] Jolene: Yeah, I totally get it. Whenever I can, I love going for walks to take it all in. And I take photos like this",
		nil},
	{"[10:04 am on 19 June, 2023] Gina: Can't wait too!",
		nil},
	{"[7:11 pm on 24 May, 2023] Evan: Of course, Sam! Painting is a great way to relieve stress and be creative. It gives you the freedom to explore colors and textures and express feelings. I've been doing it for a few years now and it helps me find peace. But unfortunately it won't help you with your weight problem, besides painting I recommend exercising!",
		[]string{"sam"}},
	{"[3:47 pm on 17 March, 2022] James: I'm always excited to combine my favorite passions: gaming and storytelling. It's great creating my own project and bringing my ideas to life, plus the challenge is really enjoyable!",
		nil},
	{"[3:31 pm on 23 August, 2023] Melanie: Wow, that sounds great - I agree, they're awesome. Here's a photo of my horse painting I did recently.",
		nil},
	{"[1:50 pm on 17 August, 2023] Caroline: Thanks, Melanie! It means a lot having you in my corner. Appreciate our friendship!",
		[]string{"melanie"}},
	{"[5:44 pm on 21 July, 2023] Jon: Thanks for the support. You rock!",
		nil},
	{"[2:34 pm on 10 July, 2022] Nate: No problem, Joanna. I'm here for you. Your hard work will pay off, I promise. Believe in yourself and your talent - you're incredible!",
		[]string{"joanna"}},
	{"[10:58 am on 9 October, 2022] Joanna: Do writing conventions exist? I'll have to look into that, it could be fun! Thanks for the idea. Have you been up to anything tonight?",
		nil},
	{"[4:06 pm on 23 January, 2023] Jolene: It is truly inspiring!",
		nil},
	{"[4:20 pm on 15 August, 2023] Sam: Thanks, Evan! I marinated it with a few different ingredients and grilled it with some veggies. It turned out really flavorful! If you want, I can share more recipes from my cooking class. Just let me know what you're looking for!",
		[]string{"evan"}},
	{"[2:24 pm on 14 August, 2023] Melanie: Thanks, Caroline! It was Matt Patterson, he is so talented! His voice and songs were amazing. What's up with you? Anything interesting going on?",
		[]string{"caroline", "matt patterson"}},
	{"[5:33 pm on 26 August, 2023] Jolene: Yeah, Deborah! We've been figuring out how to add these values into our projects. As an engineering student, I want to use my talents to do good and help solve important problems. I'm keen on coming up with new ideas and making things more efficient to make the world a better place. Going further, my mom stressed the value of helping others and that's something I want to keep in mind for my engineering projects.",
		[]string{"deborah"}},
	{"[7:44 pm on 21 April, 2022] Nate: I love this series. It has adventures, magic, and great characters - it's a must-read!",
		nil},
	{"[10:54 am on 17 November, 2023] Calvin: Having a good camera is key for capturing those special moments. What do you like to take photos of?",
		nil},
	{"[11:53 am on 23 March, 2023] Dave: That looks cozy! Where'd you find a place to stay there?",
		nil},
	{"[2:17 pm on 23 October, 2023] Calvin: Staying connected and up-to-date on world events is important to me. It helps my music stand out by incorporating unique perspectives and connects me better with my fans. Plus, it keeps me motivated and inspired.",
		nil},
	{"[7:37 pm on 9 July, 2023] Deborah: Nature helps me find peace every day - it's so refreshing!",
		nil},
	{"[3:47 pm on 17 March, 2022] James: Hey John! Video games give me tons of joy and excitement, so they keep me motivated!",
		[]string{"john"}},
	{"[6:12 pm on 14 August, 2022] Nate: No problem, Joanna! Wish them luck! Let me know how it goes. Have a blast baking!",
		[]string{"joanna"}},
	{"[5:00 pm on 11 May, 2022] John: Previously, this foundation used paper records and all inventory was recorded manually. I made an application that structured their work, and now everything they need for inventory is in one application on their smartphone.",
		nil},
}

// scoreboardGold hand-labels representative texts from the recall-quality
// scoreboard corpus templates (bench/reserve_sweep_test.go /
// bench/quality_test.go). Lowercase technical names ("pgvector") are labeled
// gold so the known miss is measured, not hidden.
var scoreboardGold = []entityGoldCase{
	{"Decision: for the database engine, the team standardized on Postgres with pgvector.",
		[]string{"postgres", "pgvector"}},
	{"In standup #3 we kept going back and forth about the auth scheme. Several people had strong opinions on the auth scheme and nobody fully agreed on the auth scheme yet.",
		nil},
	{"While we were arguing about the holiday rotation, someone mentioned that Maria took the last two weeks of December, which derailed the thread for a while.",
		[]string{"maria"}},
	{"While we were arguing about the deploy freeze calendar, someone mentioned that the freeze starts the week before Black Friday, which derailed the thread for a while.",
		[]string{"black friday"}},
	{"While we were arguing about the queue backend, someone mentioned that the vendor quote came in at 12k a year, which derailed the thread for a while.",
		nil},
	{"Decision: for the retry policy, the team standardized on exponential backoff with jitter.",
		nil},
}

// TestEntitiesPrecisionRecall scores the extractor against the hand-labeled
// corpora, micro-averaged. The floors pin the measured quality so a regression
// in the heuristics fails loudly; they are set just under the measured values,
// not aspirations.
func TestEntitiesPrecisionRecall(t *testing.T) {
	for _, set := range []struct {
		name            string
		cases           []entityGoldCase
		minPrec, minRec float64
	}{
		{name: "locomo", cases: locomoGold, minPrec: 0.90, minRec: 0.90},
		{name: "scoreboard", cases: scoreboardGold, minPrec: 0.90, minRec: 0.70},
	} {
		t.Run(set.name, func(t *testing.T) {
			var tp, fp, fn int
			for _, c := range set.cases {
				got := Entities(c.text)
				for _, g := range got {
					if slices.Contains(c.gold, g) {
						tp++
					} else {
						fp++
						t.Logf("false positive %q in %.60q", g, c.text)
					}
				}
				for _, g := range c.gold {
					if !slices.Contains(got, g) {
						fn++
						t.Logf("missed %q in %.60q", g, c.text)
					}
				}
			}
			prec, rec := 1.0, 1.0
			if tp+fp > 0 {
				prec = float64(tp) / float64(tp+fp)
			}
			if tp+fn > 0 {
				rec = float64(tp) / float64(tp+fn)
			}
			t.Logf("%s: precision %.3f (tp=%d fp=%d) recall %.3f (fn=%d)", set.name, prec, tp, fp, rec, fn)
			if prec < set.minPrec {
				t.Errorf("precision %.3f below floor %.3f", prec, set.minPrec)
			}
			if rec < set.minRec {
				t.Errorf("recall %.3f below floor %.3f", rec, set.minRec)
			}
		})
	}
}
