--------------------------- MODULE ChunkedReader ---------------------------
\* The aws-chunked decoder of sigv4/chunked.go, reading a stream an attacker
\* writes: any chunk sizes, any chunk with a signature that does not verify,
\* the stream cut or failing anywhere, a terminal chunk or trailer that is
\* wrong or missing.
\*
\* The phases and what moves the decoder between them are ChunkSteps,
\* written out from the Go table. This module adds the lengths the decoder
\* counts, which event a piece of the stream comes to (Read and nextChunk in
\* chunked.go), and what the consumer was handed.
EXTENDS Naturals, ChunkTable

CONSTANTS Declared,           \* the signed x-amz-decoded-content-length
          DeliverWithError    \* FALSE is the decoder. TRUE hands out the
                              \* bytes that came with a read error, as it
                              \* once did: checked to fail (ChunkedReaderBug.cfg)

ASSUME Declared \in Nat /\ DeliverWithError \in BOOLEAN

VARIABLES phase,      \* "start", "data", "between", "done" or "failed"
          undecoded,  \* what the declared length still allows
          remaining,  \* bytes of the current chunk not read yet
          sound,      \* the current chunk's signature will verify
          \* not in the code: what the consumer holds
          delivered,  \* bytes handed out
          unverified, \* those of them in a chunk not verified (yet)
          forged,     \* those of them in a chunk whose signature does not verify
          refused,    \* a Read failed
          ending      \* the terminal chunk and trailers the stream was
                      \* accepted on: "none", "authentic" or "forged"

vars == <<phase, undecoded, remaining, sound, delivered, unverified, forged, refused, ending>>

Phases == {"start", "data", "between", "done", "failed"}

ASSUME \A t, u \in ChunkSteps : t.from = u.from /\ t.event = u.event => t = u

Has(p, e) == \E t \in ChunkSteps : t.from = p /\ t.event = e
To(p, e) == (CHOOSE t \in ChunkSteps : t.from = p /\ t.event = e).to

\* chunkedReader.advance: an event the table does not have refuses the stream.
After(p, e) == IF Has(p, e) THEN To(p, e) ELSE "failed"

Init == /\ phase = "start"
        /\ undecoded = Declared
        /\ remaining = 0
        /\ sound = TRUE
        /\ delivered = 0
        /\ unverified = 0
        /\ forged = 0
        /\ refused = FALSE
        /\ ending = "none"

\* A Read that fails hands out nothing.
Refuse == /\ phase' = After(phase, "refused")
          /\ refused' = TRUE
          /\ UNCHANGED <<undecoded, remaining, sound, delivered, unverified, forged, ending>>

\* nextChunk: the framing up to the next chunk's data, or to the end of the
\* stream. size is what the chunk header claims; verifies whether the chunk
\* is one the signature chain accepts; closes whether what follows a
\* terminal chunk (the trailers, the declared checksum) is in order.
NextChunk(p, size, verifies, closes) ==
    IF size > undecoded THEN "failed"
    ELSE IF size > 0 THEN After(p, "opened")
    ELSE IF ~verifies \/ undecoded # 0 \/ ~closes THEN "failed"
    ELSE After(p, "terminated")

\* What a stream that ends in phase p was accepted on.
Ending(p, verifies, closes) ==
    IF p # "done" THEN ending
    ELSE IF verifies /\ closes THEN "authentic" ELSE "forged"

\* A Read that begins at a chunk boundary and reads the next header.
Header ==
    /\ phase \in {"start", "between"}
    /\ \/ Refuse    \* the CRLF or the header is malformed, or the stream ends
       \/ \E size \in 0..(Declared + 1), verifies \in BOOLEAN, closes \in BOOLEAN :
              /\ phase' = NextChunk(phase, size, verifies, closes)
              /\ refused' = (phase' = "failed")
              /\ ending' = Ending(phase', verifies, closes)
              /\ IF phase' = "data"
                 THEN /\ undecoded' = undecoded - size
                      /\ remaining' = size
                      /\ sound' = verifies
                 ELSE UNCHANGED <<undecoded, remaining, sound>>
              /\ UNCHANGED <<delivered, unverified, forged>>

\* n more bytes of the current chunk are handed out.
Forged(n) == IF sound THEN forged ELSE forged + n

\* A Read inside a chunk's data that returns n of its bytes.
Data ==
    /\ phase = "data"
    /\ \E n \in 1..remaining :
        \/ \* more of the chunk to come: handed out before its signature
           /\ n < remaining
           /\ phase' = After(phase, "read")
           /\ remaining' = remaining - n
           /\ delivered' = delivered + n
           /\ unverified' = unverified + n
           /\ forged' = Forged(n)
           /\ UNCHANGED <<undecoded, sound, refused, ending>>
        \/ \* the chunk's last bytes: its signature is checked first, and
           \* where the declared length is used up, the terminal chunk and
           \* the trailers too, all before the bytes are handed out
           /\ n = remaining
           /\ \E verifies \in BOOLEAN, closes \in BOOLEAN, size \in 0..1 :
                LET verified == After(phase, "verified")
                    ended == IF undecoded = 0
                             THEN NextChunk(verified, size, verifies, closes)
                             ELSE verified
                IN  IF sound /\ ended # "failed"
                    THEN /\ phase' = ended
                         /\ remaining' = 0
                         /\ delivered' = delivered + n
                         /\ unverified' = 0
                         /\ forged' = Forged(n)
                         /\ ending' = Ending(ended, verifies, closes)
                         /\ UNCHANGED <<undecoded, sound, refused>>
                    ELSE Refuse
        \/ \* the read fails or the stream ends, n bytes having come with it
           /\ phase' = After(phase, "refused")
           /\ refused' = TRUE
           /\ delivered' = IF DeliverWithError THEN delivered + n ELSE delivered
           /\ unverified' = IF DeliverWithError THEN unverified + n ELSE unverified
           /\ forged' = IF DeliverWithError THEN Forged(n) ELSE forged
           /\ UNCHANGED <<undecoded, remaining, sound, ending>>
    \/ /\ phase = "data"
       /\ Refuse    \* the read fails with no bytes

Next == Header \/ Data

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

TypeOK == /\ phase \in Phases
          /\ undecoded \in 0..Declared
          /\ remaining \in 0..Declared
          /\ sound \in BOOLEAN
          /\ delivered \in 0..Declared
          /\ unverified \in 0..Declared
          /\ forged \in 0..Declared

\* A stream that was refused stays refused: no later Read resumes it.
RefusalIsFinal == refused <=> phase = "failed"

\* Bytes of a chunk whose signature does not verify are handed out only
\* ahead of that signature, in a stream that is then refused: an accepted
\* stream holds none, nor does a consumer that holds the declared length.
NothingForged == /\ forged > 0 => phase \in {"data", "failed"}
                 /\ (phase = "data" /\ sound) => forged = 0

\* The decoded stream never exceeds the declared length.
Bounded == delivered <= Declared

\* A consumer that holds the declared length — where every SDK stops
\* reading — holds verified chunks only, and the stream was verified to its
\* end: nothing it holds waits for a signature it will never read up to.
FullIsVerified ==
    (Declared > 0 /\ delivered = Declared) => (phase = "done" /\ unverified = 0)

\* An accepted stream delivered exactly the declared length, all verified,
\* and ended in a terminal chunk the signature chain accepts with its
\* trailers in order: a truncated stream does not read as a complete
\* shorter one.
DoneIsComplete == phase = "done" <=> ending # "none"
DoneIsAuthentic == phase = "done" => (delivered = Declared /\ unverified = 0 /\ ending = "authentic")

\* Bytes are handed out unverified only from the chunk being read, and that
\* chunk still fits in the declared length — so a consumer reading to it
\* reaches the chunk's signature.
UnverifiedIsCurrent ==
    /\ phase \in {"start", "between", "done"} => unverified = 0
    /\ phase = "data" => /\ remaining > 0
                         /\ delivered + remaining + undecoded = Declared

=============================================================================
