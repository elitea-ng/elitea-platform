# Architecture

The handlers dispatch by name through `ROUTES`. Each handler talks to a
`NoteStore`, which extends `BaseStore`.

## Storage

`NoteStore.save` writes a note; `NoteStore.load` reads it back.

## Codec

The native `Codec` encodes note bodies before they are stored.
