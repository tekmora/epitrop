# Pull Request

## Description

<!-- Summarize the changes and their architectural rationale. -->

## Pre-Merge Verification Checklist

- [ ] Local verification passed: `make verify-all`
- [ ] No new HISS / NASA Power-of-10 infractions (functions $\le 60$ LOC)
- [ ] 3D Tests included (Positive, Negative, Boundary) for public APIs
- [ ] Agent contexts in sync: `standardsctl compile-context --verify`
- [ ] Commit messages adhere to Conventional Commits format
