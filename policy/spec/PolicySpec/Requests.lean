import PolicySpec.Properties

/-! What holds of whole requests: several needs, a copy, many keys. -/

namespace PolicySpec

/-- A request is admitted exactly when its bucket exists and is visible to
the requester, each thing it needs is permitted, and its copy source, if it
has one, is readable. -/
theorem admits_iff (w : World) (who : Requester) (op : Operation) :
    w.admits who op = true ↔
      ∃ p, w.find op.bucket = some p ∧
        p.visible who.principal = true ∧
        (∀ n ∈ op.needs, who.may p n = true) ∧
        (∀ c, op.copy = some c → w.copyReadable who c = true) := by
  unfold World.admits
  cases w.find op.bucket with
  | none => simp
  | some p => cases op.copy <;> simp [and_assoc]

/-- A copy's source is readable exactly when its bucket exists, belongs to
the requester's own tenant, and the requester may read the object. -/
theorem copyReadable_iff (w : World) (who : Requester) (c : CopySource) :
    w.copyReadable who c = true ↔
      ∃ src, w.find c.bucket = some src ∧
        src.owner = who.principal.tenant ∧ who.may src c.need = true := by
  unfold World.copyReadable
  cases w.find c.bucket <;> simp

/-- A bucket that is not there for the requester admits no request,
whatever the request is. -/
theorem hidden_bucket_admits_nothing (w : World) (who : Requester) (op : Operation) (p : Policy)
    (hfind : w.find op.bucket = some p) (hhidden : p.visible who.principal = false) :
    w.admits who op = false := by
  simp [World.admits, hfind, hhidden]

/-- Everything an admitted request needs is permitted: no need is skipped. -/
theorem admitted_needs_permitted (w : World) (who : Requester) (op : Operation) (p : Policy)
    (hfind : w.find op.bucket = some p) (hadmit : w.admits who op = true) :
    ∀ n ∈ op.needs, who.may p n = true := by
  obtain ⟨q, hq, _, hneeds, _⟩ := (admits_iff w who op).mp hadmit
  rw [hfind] at hq
  cases hq
  exact hneeds

/-- A request that needs more is admitted no more often: an action a header
adds can only refuse. -/
theorem more_needs_admit_less (w : World) (who : Requester) (bucket : String)
    (needs : List Need) (extra : Need) (copy : Option CopySource)
    (hadmit : w.admits who ⟨bucket, extra :: needs, copy⟩ = true) :
    w.admits who ⟨bucket, needs, copy⟩ = true := by
  rw [admits_iff] at hadmit ⊢
  obtain ⟨p, hfind, hvisible, hneeds, hcopy⟩ := hadmit
  exact ⟨p, hfind, hvisible, fun n hn => hneeds n (List.mem_cons_of_mem _ hn), hcopy⟩

/-- A requester of another tenant is admitted only when, for each thing the
request needs, an Allow statement of the bucket's policy applies. -/
theorem foreign_admitted_by_allows (w : World) (who : Requester) (op : Operation) (p : Policy)
    (hfind : w.find op.bucket = some p) (hforeign : who.principal.tenant ≠ p.owner)
    (hadmit : w.admits who op = true) :
    ∀ n ∈ op.needs, ∃ s ∈ p.statements, s.effect = .allow ∧
      s.applies ⟨who.principal, n.action, n.resource, who.source⟩ = true := by
  intro n hn
  exact foreign_needs_allow who.userPolicy p _ hforeign
    (admitted_needs_permitted w who op p hfind hadmit n hn)

/-- A copy never reads from another tenant's bucket: the source of an
admitted copy belongs to the requester's own tenant, whatever any bucket's
policy grants. -/
theorem copy_source_is_own_tenant (w : World) (who : Requester) (op : Operation)
    (c : CopySource) (src : Policy)
    (hcopy : op.copy = some c) (hsrc : w.find c.bucket = some src)
    (hadmit : w.admits who op = true) :
    src.owner = who.principal.tenant := by
  obtain ⟨_, _, _, _, hreadable⟩ := (admits_iff w who op).mp hadmit
  obtain ⟨src', hsrc', howner, _⟩ := (copyReadable_iff w who c).mp (hreadable c hcopy)
  rw [hsrc] at hsrc'
  cases hsrc'
  exact howner

/-- DeleteObjects answers for a key what a request for that key alone would
be answered, whatever the other keys are. -/
theorem deletes_each_as_alone (w : World) (who : Requester) (bucket : String)
    (keys : List (List Need)) (verdicts : List Bool)
    (h : w.deletes who bucket keys = some verdicts) :
    verdicts = keys.map fun needs => w.admits who ⟨bucket, needs, none⟩ := by
  unfold World.deletes at h
  cases hfind : w.find bucket with
  | none => simp [hfind] at h
  | some p =>
    simp only [hfind] at h
    split at h
    · rename_i hvisible
      cases h
      simp [World.admits, hfind, hvisible]
    · cases h

/-- DeleteObjects refuses the whole request exactly when the bucket is not
there for the requester; it never refuses it for a key. -/
theorem deletes_none_iff (w : World) (who : Requester) (bucket : String)
    (keys : List (List Need)) :
    w.deletes who bucket keys = none ↔
      ∀ p, w.find bucket = some p → p.visible who.principal = false := by
  unfold World.deletes
  cases w.find bucket with
  | none => simp
  | some p => cases h : p.visible who.principal <;> simp [h]

end PolicySpec
