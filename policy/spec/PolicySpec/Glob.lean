import PolicySpec.Semantics

/-! What a pattern matches, said without saying how to find out. -/

namespace PolicySpec

/-- A pattern matches a value when the value can be read off the pattern:
`*` as any run of characters, `?` as any one character, and any other
character as itself. -/
inductive Matches : List Char → List Char → Prop
  | nil : Matches [] []
  | star (run : List Char) {p v : List Char} : Matches p v → Matches ('*' :: p) (run ++ v)
  | one (d : Char) {p v : List Char} : Matches p v → Matches ('?' :: p) (d :: v)
  | same (c : Char) {p v : List Char} : c ≠ '*' → c ≠ '?' → Matches p v → Matches (c :: p) (c :: v)

theorem glob_star_cons (p : List Char) (c : Char) (v : List Char) :
    glob ('*' :: p) (c :: v) = (glob p (c :: v) || glob ('*' :: p) v) := by
  simp [glob]

theorem glob_star_run (run : List Char) {p v : List Char} (h : glob p v = true) :
    glob ('*' :: p) (run ++ v) = true := by
  induction run with
  | nil =>
    cases v with
    | nil => simpa [glob] using h
    | cons c v => simp [glob_star_cons, h]
  | cons c run ih => simp [glob_star_cons, ih]

theorem Matches.of_glob {p v : List Char} (h : glob p v = true) : Matches p v := by
  fun_induction glob p v with
  | case1 v =>
    have : v = [] := by simpa using h
    exact this ▸ .nil
  | case2 p ih => exact .star [] (ih h)
  | case3 p c v ih1 ih2 =>
    simp only [Bool.or_eq_true] at h
    rcases h with h | h
    · exact .star [] (ih1 h)
    · have m := ih2 h
      generalize hq : '*' :: p = q at m
      cases m with
      | nil => cases hq
      | star run m => cases hq; exact .star (c :: run) m
      | one d m => cases hq
      | same c' hs _ m => cases hq; exact absurd rfl hs
  | case4 => cases h
  | case5 c p d v hc ih =>
    simp only [Bool.and_eq_true, Bool.or_eq_true, beq_iff_eq] at h
    obtain ⟨hcd, hrest⟩ := h
    rcases hcd with rfl | rfl
    · exact .one d (ih hrest)
    · by_cases hq : c = '?'
      · exact hq ▸ .one c (ih hrest)
      · exact .same c hc hq (ih hrest)

theorem Matches.glob {p v : List Char} (h : Matches p v) : glob p v = true := by
  induction h with
  | nil => simp [PolicySpec.glob]
  | star run _ ih => exact glob_star_run run ih
  | one d _ ih => simp [PolicySpec.glob, ih]
  | same c hs hq _ ih =>
    unfold PolicySpec.glob
    split <;> simp_all

/-- `glob` computes exactly what a pattern matches. -/
theorem glob_iff (p v : List Char) : glob p v = true ↔ Matches p v :=
  ⟨Matches.of_glob, Matches.glob⟩

/-- A character that stands for itself in a pattern. -/
def Literal (c : Char) : Prop := c ≠ '*' ∧ c ≠ '?'

theorem Matches.refl {p : List Char} (h : ∀ c ∈ p, Literal c) : Matches p p := by
  induction p with
  | nil => exact .nil
  | cons c p ih =>
    have hc := h c (by simp)
    exact .same c hc.1 hc.2 (ih fun d hd => h d (by simp [hd]))

/-- `*` alone matches every value, the empty one included. -/
theorem matches_star (v : List Char) : Matches ['*'] v := by
  simpa using Matches.star v .nil

/-- A pattern without wildcards matches itself and nothing else. -/
theorem matches_literal {p : List Char} (h : ∀ c ∈ p, Literal c) (v : List Char) :
    Matches p v ↔ p = v := by
  constructor
  · intro m
    induction m with
    | nil => rfl
    | star run _ _ => exact absurd rfl (h '*' (by simp)).1
    | one d _ _ => exact absurd rfl (h '?' (by simp)).2
    | same c _ _ _ ih => rw [ih fun d hd => h d (by simp [hd])]
  · rintro rfl
    exact Matches.refl h

/-- A pattern of literal characters followed by `*` matches exactly the
values that start with those characters: `b/logs/*` is everything under
`b/logs/`. -/
theorem matches_prefix {p : List Char} (h : ∀ c ∈ p, Literal c) (v : List Char) :
    Matches (p ++ ['*']) v ↔ p <+: v := by
  induction p generalizing v with
  | nil => simpa using matches_star v
  | cons c p ih =>
    have hc := h c (by simp)
    have hp : ∀ d ∈ p, Literal d := fun d hd => h d (by simp [hd])
    constructor
    · intro m
      generalize hq : c :: p ++ ['*'] = q at m
      cases m with
      | nil => cases hq
      | star run _ => cases hq; exact absurd rfl hc.1
      | one d _ => cases hq; exact absurd rfl hc.2
      | same c' _ _ m =>
        cases hq
        exact List.cons_prefix_cons.mpr ⟨rfl, (ih hp _).mp m⟩
    · rintro ⟨t, rfl⟩
      exact .same c hc.1 hc.2 ((ih hp _).mpr ⟨t, rfl⟩)

/-- A resource pattern matches a resource as `Matches` says, character for
character. -/
theorem matchResource_iff (pattern value : String) :
    matchResource pattern value = true ↔ Matches pattern.toList value.toList :=
  glob_iff _ _

/-- An action pattern matches an action as `Matches` says once `A`–`Z` are
read as `a`–`z` on both sides. -/
theorem matchAction_iff (pattern value : String) :
    matchAction pattern value = true ↔
      Matches (pattern.toList.map Char.toLower) (value.toList.map Char.toLower) :=
  glob_iff _ _

/-- Two spellings of an action that differ only in the case of ASCII
letters are the same action, to every pattern. -/
theorem matchAction_case (pattern a b : String)
    (h : a.toList.map Char.toLower = b.toList.map Char.toLower) :
    matchAction pattern a = matchAction pattern b := by
  simp [matchAction, h]

end PolicySpec
