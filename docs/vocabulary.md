# Vocabulary

Each word is shown through visible dimensions for word choice:
- **meaning** — what the word basically means
- **pressure** — what kind of situation or idea pulls this word into use
- **tone** — how it sounds or feels
- **nearby alternatives** — similar words and how they differ
- **where it breaks** — common wrong substitutions or contexts where the word stops fitting

---

## abstraction barrier
- **meaning:** a boundary that hides inner details behind a stable interface
- **pressure:** use it when the real point is not separation by folder, but protection of meaning across a boundary
- **tone:** technical, precise, architecture-heavy
- **nearby alternatives:** `interface` is smaller; `boundary` is broader; `seam` stresses test/change points; `abstraction barrier` stresses what must not leak across
- **where it breaks:** too heavy for ordinary module splits; do not use it when you only mean a package border or function signature

## academic
- **meaning:** related to scholarship, school, or theory-centered learning
- **pressure:** use it when the habit is read-first, theory-first, or detached from building pressure
- **tone:** neutral by default, but can become mildly critical in engineering discussion
- **nearby alternatives:** `scholarly` is more formal and positive; `theoretical` focuses on ideas; `academic` can imply classroom-style distance from practice
- **where it breaks:** do not use it as a general synonym for `smart` or `formal`; avoid `acadamical`

## amortize
- **meaning:** spread one cost across many operations
- **pressure:** use it when repeated work becomes cheaper because multiple actions share one expensive step
- **tone:** technical, optimization-oriented
- **nearby alternatives:** `reduce cost` is generic; `batch` describes the mechanism; `amortize` describes the cost shape over time
- **where it breaks:** do not use it when cost is simply lower once; it needs repeated operations sharing a burden

## beneath
- **meaning:** lower than something, or hidden under it
- **pressure:** use it when describing a deeper layer under a visible surface, especially in writing about ideas
- **tone:** slightly formal, literary-technical
- **nearby alternatives:** `underneath` is more natural and visual; `below` is more literal; `beneath` feels more reflective
- **where it breaks:** can sound too formal in casual speech; do not force it where plain `under` is enough

## coherent
- **meaning:** logically connected and consistent as a whole
- **pressure:** use it when many parts fit together without contradiction
- **tone:** thoughtful, evaluative, design-oriented
- **nearby alternatives:** `consistent` is narrower; `clear` is about readability; `coherent` says the whole shape holds together
- **where it breaks:** do not use it when you only mean `easy to read`; a design can be clear but not coherent

## compile
- **meaning:** gather items into a structured collection
- **pressure:** use it when information is being assembled, not merely noticed
- **tone:** neutral, organized, methodical
- **nearby alternatives:** `collect` is looser; `gather` is broader; `compile` suggests deliberate arrangement
- **where it breaks:** outside code, do not confuse it with `build`; `compile examples` works, `compile an opinion` usually does not

## conclude
- **meaning:** finish something, or reach a judgment through reasoning
- **pressure:** use it when evidence or argument reaches an endpoint
- **tone:** formal to neutral, reasoned
- **nearby alternatives:** `decide` is broader and more active; `infer` stresses deduction; `conclude` stresses arriving at a final view
- **where it breaks:** do not use it when the statement is only a guess; `conclude` needs support

## crucially
- **meaning:** to a decisive degree; in a way that is critically important
- **pressure:** use it to mark the one point the argument depends on
- **tone:** emphatic but still formal
- **nearby alternatives:** `importantly` is weaker; `critically` is similar but slightly harsher; `crucially` often fits reasoning best
- **where it breaks:** too strong for minor details; if everything is crucial, nothing is

## discontinuity
- **meaning:** a break in continuity; a place where smooth connection is lost
- **pressure:** use it when something that should carry through instead breaks across time, layers, or states
- **tone:** technical, analytical
- **nearby alternatives:** `gap` is simpler; `break` is more general; `discontinuity` highlights the loss of a continuous line
- **where it breaks:** too heavy for small missing details; use it when continuity itself matters

## frustrated
- **meaning:** upset or discouraged because progress is blocked or harder than expected
- **pressure:** use it when there is effort plus obstruction
- **tone:** emotional but controlled
- **nearby alternatives:** `annoyed` is lighter; `discouraged` is sadder; `frustrated` keeps the sense of blocked effort
- **where it breaks:** do not use it for deep grief or general depression; it is tied to blockage

## heuristic
- **meaning:** a practical rule of thumb used to guide action or judgment
- **pressure:** use it when something is useful but not a proof or full theory
- **tone:** technical, exploratory
- **nearby alternatives:** `rule of thumb` is more casual; `approximation` is more mathematical; `heuristic` is the engineering-thinking word
- **where it breaks:** do not present a heuristic as a guarantee; that is exactly where the word matters

## hesitant
- **meaning:** uncertain or reluctant to act
- **pressure:** use it when the person is leaning back, not fully refusing
- **tone:** soft, inward, reflective
- **nearby alternatives:** `reluctant` is stronger; `uncertain` is more cognitive; `hesitant` carries a pause before action
- **where it breaks:** too mild for hard refusal; too soft for fear or panic

## hugely
- **meaning:** to a great extent
- **pressure:** use it as an informal intensifier when something matters a lot
- **tone:** conversational, emphatic, not formal
- **nearby alternatives:** `greatly` is more formal; `massively` is stronger and rougher; `hugely` is natural in speech
- **where it breaks:** avoid it in formal technical writing when `significantly` or `substantially` is cleaner

## idempotent
- **meaning:** producing the same result even if applied more than once
- **pressure:** use it when repetition should not change the outcome after the first successful effect
- **tone:** technical, mathematical, precise
- **nearby alternatives:** `safe to repeat` is plainer; `duplicate-tolerant` is narrower; `idempotent` is the standard word when repeated application preserves the same state
- **where it breaks:** do not use it just to mean `harmless`; it specifically means repeated application does not create an additional effect beyond the first one
- **example:** Setting the same ack flag to `true` twice is idempotent because the tracker state does not change after the first update.

## hypothesis
- **meaning:** a testable claim formed before examining evidence
- **pressure:** use it when you want a statement that can be confirmed or disproved
- **tone:** scientific, disciplined
- **nearby alternatives:** `guess` is casual; `assumption` may stay untested; `hypothesis` implies testing
- **where it breaks:** do not use it for beliefs that are never exposed to evidence

## legitimate
- **meaning:** valid, justified, or properly earned under the relevant rules
- **pressure:** use it when something must be more than convenient or merely possible; it must be rightful under the system
- **tone:** strong, serious, rule-aware
- **nearby alternatives:** `valid` is narrower; `legal` is about law; `proper` is softer; `legitimate` carries legitimacy under a governing logic
- **where it breaks:** do not reduce it to `nice` or `reasonable`; it implies rightful standing

## mature
- **meaning:** fully developed through time, pressure, and correction
- **pressure:** use it when something looks simple because it has already absorbed hard lessons
- **tone:** respectful, seasoned, high-trust
- **nearby alternatives:** `experienced` fits people more; `refined` stresses polish; `stable` stresses reliability; `mature` carries all of these with time-pressure behind them
- **where it breaks:** do not use it for something merely old; age alone does not make a system mature

## north star
- **meaning:** the main long-term guiding goal
- **pressure:** use it when many local decisions need one stable direction
- **tone:** strategic, guiding, slightly metaphorical
- **nearby alternatives:** `goal` is flatter; `vision` is broader; `principle` is more static; `north star` highlights orientation over time
- **where it breaks:** too grand for small short-lived tasks; use it for a larger throughline

## obstacle
- **meaning:** something that blocks progress or makes movement harder
- **pressure:** use it when there is an actual barrier, not just inconvenience
- **tone:** neutral, problem-focused
- **nearby alternatives:** `problem` is broader; `barrier` is stronger; `obstacle` fits something in the path that must be worked around or removed
- **where it breaks:** too dramatic for a tiny annoyance; not every difficulty is an obstacle

## originate
- **meaning:** begin, arise, or come from a source
- **pressure:** use it when source and authority matter
- **tone:** formal, source-aware
- **nearby alternatives:** `start` is simpler; `come from` is casual; `originate` stresses the point of origin
- **where it breaks:** can sound stiff in casual speech; use it when source is the important part

## polish
- **meaning:** refine or improve something that already works
- **pressure:** use it when structure exists and now needs sharper fit, cleaner wording, or better finish
- **tone:** constructive, finishing-stage
- **nearby alternatives:** `improve` is broad; `refine` is slightly more formal; `polish` adds the sense of final smoothing
- **where it breaks:** do not use it for major redesign; polishing is not rebuilding

## pollute
- **meaning:** make something impure, contaminated, or cluttered
- **pressure:** use it when one concern dirties another boundary or introduces unwanted noise
- **tone:** strong, negative, boundary-protective
- **nearby alternatives:** `clutter` is weaker; `contaminate` is harsher; `pollute` sits well in design criticism
- **where it breaks:** too strong for small imperfections; use it when the foreign concern truly damages the shape

## profoundly
- **meaning:** deeply and in a way that changes understanding at a serious level
- **pressure:** use it when the effect is not just large, but deep and transformative
- **tone:** serious, reflective, weighty
- **nearby alternatives:** `deeply` is simpler; `greatly` is flatter; `profoundly` carries depth plus importance
- **where it breaks:** too heavy for ordinary improvements; reserve it for real shifts in understanding or impact

## prose
- **meaning:** ordinary written language used for explanation or narrative
- **pressure:** use it when distinguishing explanation from code, poetry, or bullet fragments
- **tone:** literary but common in writing discussions
- **nearby alternatives:** `writing` is broader; `text` is flatter; `prose` highlights explanatory language as a form
- **where it breaks:** do not use it when plain `writing` is enough and no contrast in form is needed

## rationale
- **meaning:** the underlying reason or justification for a choice
- **pressure:** use it when the why matters more than the what
- **tone:** formal, analytical, design-review friendly
- **nearby alternatives:** `reason` is broader; `motivation` is more personal; `rationale` is the design and decision word
- **where it breaks:** can sound too formal in casual speech; use it when explicit justification matters

## seam
- **meaning:** a boundary where two parts meet
- **pressure:** use it when the connection point is useful for testing, changing, or reasoning about ownership
- **tone:** technical but metaphorical, elegant
- **nearby alternatives:** `boundary` is broader; `interface` is more explicit; `seam` suggests a join that can be opened or examined
- **where it breaks:** do not use it for every border; use it when the join itself matters

## separate
- **meaning:** divide apart or keep distinct
- **pressure:** use it when mixed concerns need to be pulled apart
- **tone:** neutral, structural
- **nearby alternatives:** `split` is more forceful; `distinguish` is more conceptual; `separate` is the plain structural verb
- **where it breaks:** too plain when the issue is not division but ownership or abstraction

## speculative
- **meaning:** based on guessing or theorizing without evidence
- **pressure:** use it when a design choice solves future pressure that has not appeared yet
- **tone:** mildly critical, cautionary
- **nearby alternatives:** `theoretical` is more neutral; `hypothetical` is broader; `speculative` suggests building on uncertain ground
- **where it breaks:** do not use it for any future thinking; it specifically warns about low-evidence design

## throughline
- **meaning:** the main connecting idea running through a larger work
- **pressure:** use it when many local topics need one unifying thread
- **tone:** reflective, narrative, structural
- **nearby alternatives:** `theme` is broader; `thread` is simpler; `throughline` strongly implies continuity across the whole sequence
- **where it breaks:** too literary for tiny local points; best for episode chains, docs, or long arguments

## underneath
- **meaning:** below the surface or under something
- **pressure:** use it when you want a natural, visual sense of hidden depth
- **tone:** natural, less formal than `beneath`
- **nearby alternatives:** `beneath` is more formal; `below` is more literal; `underneath` feels more physical and intuitive
- **where it breaks:** can sound too physical in highly formal prose; then `beneath` may fit better

## vague
- **meaning:** unclear in meaning, boundary, or intent
- **pressure:** use it when an idea is not sharp enough to guide action or judgment
- **tone:** critical but not harsh
- **nearby alternatives:** `fuzzy` is more visual or tactile; `unclear` is plainer; `vague` is the right word for imprecise ideas
- **where it breaks:** do not use it for blurry images or soft outlines; that is where `fuzzy` fits better

## widen
- **meaning:** make something broader in scope, meaning, or coverage
- **pressure:** use it when a design, interface, or episode starts handling more cases or responsibility
- **tone:** neutral, often cautionary in design talk
- **nearby alternatives:** `expand` is broader; `broaden` is close; `widen` often feels spatial and useful for scope creep or interface growth
- **where it breaks:** not ideal when growth is vertical depth rather than broader coverage
