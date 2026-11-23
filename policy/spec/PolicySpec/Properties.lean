import PolicySpec.Semantics

/-! What the semantics guarantee, for every policy and every request. -/

namespace PolicySpec

/-! ## Principals -/

/-- `tenant/user` names that user of that tenant. -/
theorem user_entry_covers (t u : String) (who : Principal) :
    (Entry.user t u).covers who = true ↔ who.tenant = t ∧ who.user = u := by
  simp only [Entry.covers, Bool.and_eq_true, beq_iff_eq]
  exact ⟨fun ⟨a, b⟩ => ⟨a.symm, b.symm⟩, fun ⟨a, b⟩ => ⟨a.symm, b.symm⟩⟩

/-- `tenant/*` names every user of that tenant, and nobody else's. -/
theorem tenant_entry_covers (t : String) (who : Principal) :
    (Entry.tenant t).covers who = true ↔ who.tenant = t := by
  simp only [Entry.covers, beq_iff_eq]
  exact eq_comm

theorem everyone_includes (who : Principal) : Subject.everyone.includes who = true := rfl

/-- `Principal` is about the principals some entry names. -/
theorem listed_includes (es : List Entry) (who : Principal) :
    (Subject.listed es).includes who = true ↔ ∃ e ∈ es, e.covers who = true := by
  simp [Subject.includes]

/-- `NotPrincipal` is about the principals no entry names. -/
theorem allBut_includes (es : List Entry) (who : Principal) :
    (Subject.allBut es).includes who = true ↔ ∀ e ∈ es, e.covers who = false := by
  simp [Subject.includes]

/-! ## Conditions -/

/-- With the source address unknown, a condition holds exactly when it has no
`IpAddress`: `IpAddress` fails, `NotIpAddress` holds. -/
theorem condition_unknown_source (c : Condition) :
    c.holds none = true ↔ c.ipAddress = [] := by
  simp [Condition.holds]

/-- With the source address known, a condition holds exactly when the
address is in one of the `IpAddress` ranges, if there are any, and in none
of the `NotIpAddress` ranges. -/
theorem condition_known_source (c : Condition) (a : Addr) :
    c.holds (some a) = true ↔
      (c.ipAddress = [] ∨ ∃ v ∈ c.ipAddress, contains v a = true) ∧
      (∀ v ∈ c.notIpAddress, contains v a = false) := by
  cases h : c.notIpAddress <;> simp [Condition.holds, h]

/-! ## The bucket policy's decision -/

/-- Statement `i` is the first statement of the effect that applies to the
request. -/
def Policy.FirstApplying (p : Policy) (e : Effect) (r : Request) (i : Nat) : Prop :=
  ∃ h : i < p.statements.length,
    (p.statements[i].effect = e ∧ p.statements[i].applies r = true) ∧
    ∀ (j : Nat) (hj : j < i),
      ¬(p.statements[j].effect = e ∧ p.statements[j].applies r = true)

theorem first_eq_some_iff (p : Policy) (e : Effect) (r : Request) (i : Nat) :
    p.first e r = some i ↔ p.FirstApplying e r i := by
  simp [Policy.first, Policy.FirstApplying, List.findIdx?_eq_some_iff_getElem]

theorem first_isSome_iff (p : Policy) (e : Effect) (r : Request) :
    (p.first e r).isSome ↔ ∃ s ∈ p.statements, s.effect = e ∧ s.applies r = true := by
  simp [Policy.first, List.findIdx?_isSome]

theorem first_eq_none_iff (p : Policy) (e : Effect) (r : Request) :
    p.first e r = none ↔ ∀ s ∈ p.statements, s.effect = e → s.applies r = false := by
  simp [Policy.first, List.findIdx?_eq_none_iff]

/-- The policy denies by statement `i` exactly when `i` is the first Deny
statement that applies. -/
theorem decide_deny_iff (p : Policy) (r : Request) (i : Nat) :
    p.decide r = .deny i ↔ p.FirstApplying .deny r i := by
  rw [← first_eq_some_iff]
  unfold Policy.decide
  cases p.first .deny r <;> cases p.first .allow r <;> simp

/-- The policy allows by statement `i` exactly when no Deny statement
applies and `i` is the first Allow statement that does. -/
theorem decide_allow_iff (p : Policy) (r : Request) (i : Nat) :
    p.decide r = .allow i ↔
      (∀ s ∈ p.statements, s.effect = .deny → s.applies r = false) ∧
      p.FirstApplying .allow r i := by
  rw [← first_eq_some_iff, ← first_eq_none_iff]
  unfold Policy.decide
  cases p.first .deny r <;> cases p.first .allow r <;> simp

/-- The policy decides nothing exactly when no statement applies. -/
theorem decide_none_iff (p : Policy) (r : Request) :
    p.decide r = .none ↔ ∀ s ∈ p.statements, s.applies r = false := by
  have h : (∀ s ∈ p.statements, s.applies r = false) ↔
      p.first .deny r = none ∧ p.first .allow r = none := by
    simp only [first_eq_none_iff]
    constructor
    · exact fun h => ⟨fun s hs _ => h s hs, fun s hs _ => h s hs⟩
    · intro ⟨hd, ha⟩ s hs
      cases he : s.effect
      · exact ha s hs he
      · exact hd s hs he
  rw [h]
  unfold Policy.decide
  cases p.first .deny r <;> cases p.first .allow r <;> simp

/-- An applying Deny statement decides, whatever else the policy says, and
the decision names the first one. -/
theorem deny_wins (p : Policy) (r : Request) (s : Statement)
    (hmem : s ∈ p.statements) (hdeny : s.effect = .deny) (happ : s.applies r = true) :
    ∃ i, p.decide r = .deny i ∧ p.FirstApplying .deny r i := by
  have h : (p.first .deny r).isSome := (first_isSome_iff p .deny r).mpr ⟨s, hmem, hdeny, happ⟩
  obtain ⟨i, hi⟩ := Option.isSome_iff_exists.mp h
  have hfirst := (first_eq_some_iff p .deny r i).mp hi
  exact ⟨i, (decide_deny_iff p r i).mpr hfirst, hfirst⟩

/-! ## Visibility -/

/-- A policy mentions a principal exactly when an Allow statement's
`Principal` is everyone or lists an entry naming it. -/
theorem mentions_iff (p : Policy) (who : Principal) :
    p.mentions who = true ↔
      ∃ s ∈ p.statements, s.effect = .allow ∧
        (s.subject = .everyone ∨ ∃ es, s.subject = .listed es ∧ ∃ e ∈ es, e.covers who = true) := by
  simp only [Policy.mentions, List.any_eq_true, Bool.and_eq_true, beq_iff_eq]
  refine exists_congr fun s => and_congr_right fun _ => and_congr_right fun _ => ?_
  cases s.subject <;> simp

/-- The visibility gate hides no bucket its policy grants: an Allow statement
that applies to a request also mentions the requester. -/
theorem allow_applies_mentions (p : Policy) (hwf : p.WellFormed) (r : Request) (s : Statement)
    (hmem : s ∈ p.statements) (hallow : s.effect = .allow) (happ : s.applies r = true) :
    p.mentions r.principal = true := by
  rw [mentions_iff]
  refine ⟨s, hmem, hallow, ?_⟩
  have hsub := hwf s hmem hallow
  simp only [Statement.applies, Bool.and_eq_true] at happ
  have hinc := happ.1.1.1
  cases hs : s.subject with
  | everyone => exact Or.inl rfl
  | listed es => exact Or.inr ⟨es, rfl, by simpa [hs, Subject.includes] using hinc⟩
  | allBut es => exact absurd hs (hsub es)

/-! ## The user policy -/

/-- A user policy allows an action exactly when it is empty, or no Deny
statement matches the action and an Allow statement does. -/
theorem userAllows_iff (up : List UserStatement) (action : String) :
    userAllows up action = true ↔
      up = [] ∨
      ((∀ s ∈ up, s.effect = .deny → s.applies action = false) ∧
        ∃ s ∈ up, s.effect = .allow ∧ s.applies action = true) := by
  simp [userAllows]

/-- A matching Deny in a user policy refuses the action. -/
theorem user_deny_wins (up : List UserStatement) (action : String) (s : UserStatement)
    (hmem : s ∈ up) (hdeny : s.effect = .deny) (happ : s.applies action = true) :
    userAllows up action = false := by
  apply Bool.eq_false_iff.mpr
  intro h
  rcases (userAllows_iff up action).mp h with hnil | ⟨hnodeny, _⟩
  · simp [hnil] at hmem
  · simp [hnodeny s hmem hdeny] at happ

/-! ## The gateway's decision -/

/-- The gateway's decision, statement by statement: the user policy allows
the action, the bucket is visible, no Deny statement applies, and the
requester is of the owning tenant or an Allow statement applies. -/
theorem permits_iff (up : List UserStatement) (p : Policy) (r : Request) :
    permits up p r = true ↔
      userAllows up r.action = true ∧
      p.visible r.principal = true ∧
      (∀ s ∈ p.statements, s.effect = .deny → s.applies r = false) ∧
      (r.principal.tenant = p.owner ∨
        ∃ s ∈ p.statements, s.effect = .allow ∧ s.applies r = true) := by
  rw [← first_eq_none_iff, ← first_isSome_iff]
  unfold permits Policy.decide
  cases p.first .deny r <;> cases p.first .allow r <;> simp [and_assoc]

/-- An applying Deny statement refuses the request, whoever asks. -/
theorem deny_refuses (up : List UserStatement) (p : Policy) (r : Request) (s : Statement)
    (hmem : s ∈ p.statements) (hdeny : s.effect = .deny) (happ : s.applies r = true) :
    permits up p r = false := by
  apply Bool.eq_false_iff.mpr
  intro h
  have := ((permits_iff up p r).mp h).2.2.1 s hmem hdeny
  simp [happ] at this

/-- A user of the owning tenant is admitted exactly when the user policy
allows the action and no Deny statement applies. -/
theorem owner_baseline (up : List UserStatement) (p : Policy) (r : Request)
    (hown : r.principal.tenant = p.owner) :
    permits up p r = true ↔
      userAllows up r.action = true ∧
      ∀ s ∈ p.statements, s.effect = .deny → s.applies r = false := by
  simp [permits_iff, Policy.visible, hown]

/-- A user of another tenant is admitted exactly when the user policy allows
the action, the bucket is visible, no Deny statement applies and an Allow
statement does. -/
theorem foreign_baseline (up : List UserStatement) (p : Policy) (r : Request)
    (hforeign : r.principal.tenant ≠ p.owner) :
    permits up p r = true ↔
      userAllows up r.action = true ∧
      p.mentions r.principal = true ∧
      (∀ s ∈ p.statements, s.effect = .deny → s.applies r = false) ∧
      ∃ s ∈ p.statements, s.effect = .allow ∧ s.applies r = true := by
  simp [permits_iff, Policy.visible, hforeign]

/-- A requester of another tenant is admitted only by an Allow statement that
applies to the request. -/
theorem foreign_needs_allow (up : List UserStatement) (p : Policy) (r : Request)
    (hforeign : r.principal.tenant ≠ p.owner) (hpermit : permits up p r = true) :
    ∃ s ∈ p.statements, s.effect = .allow ∧ s.applies r = true :=
  ((foreign_baseline up p r hforeign).mp hpermit).2.2.2

/-- Without a policy statement, only the owning tenant gets in. -/
theorem empty_policy_is_private (up : List UserStatement) (owner : String) (r : Request)
    (hpermit : permits up ⟨owner, []⟩ r = true) :
    r.principal.tenant = owner := by
  simpa using ((permits_iff up ⟨owner, []⟩ r).mp hpermit).2.2.2

/-- Adding a Deny statement anywhere never admits a request that was refused. -/
theorem adding_deny_admits_nothing (up : List UserStatement) (owner : String)
    (before after : List Statement) (d : Statement) (r : Request)
    (hdeny : d.effect = .deny)
    (hpermit : permits up ⟨owner, before ++ d :: after⟩ r = true) :
    permits up ⟨owner, before ++ after⟩ r = true := by
  rw [permits_iff] at hpermit ⊢
  obtain ⟨huser, hvisible, hnodeny, hallow⟩ := hpermit
  refine ⟨huser, ?_, ?_, ?_⟩
  · simpa [Policy.visible, Policy.mentions, hdeny] using hvisible
  · intro s hs
    refine hnodeny s ?_
    simp only [List.mem_append, List.mem_cons] at hs ⊢
    exact hs.imp_right Or.inr
  · refine hallow.imp_right ?_
    rintro ⟨s, hs, hsallow, hsapp⟩
    refine ⟨s, ?_, hsallow, hsapp⟩
    simp only [List.mem_append, List.mem_cons] at hs ⊢
    rcases hs with h | rfl | h
    · exact Or.inl h
    · simp [hdeny] at hsallow
    · exact Or.inr h

/-- Adding an Allow statement anywhere changes nothing for the owning
tenant's users. -/
theorem adding_allow_leaves_owner (up : List UserStatement) (owner : String)
    (before after : List Statement) (a : Statement) (r : Request)
    (hallow : a.effect = .allow) (hown : r.principal.tenant = owner) :
    permits up ⟨owner, before ++ a :: after⟩ r = permits up ⟨owner, before ++ after⟩ r := by
  rw [Bool.eq_iff_iff, owner_baseline _ _ _ hown, owner_baseline _ _ _ hown]
  refine and_congr_right fun _ => ?_
  simp only [List.mem_append, List.mem_cons]
  constructor
  · exact fun h s hs => h s (hs.imp_right Or.inr)
  · intro h s hs hsdeny
    rcases hs with hs | rfl | hs
    · exact h s (Or.inl hs) hsdeny
    · simp [hallow] at hsdeny
    · exact h s (Or.inr hs) hsdeny

end PolicySpec
