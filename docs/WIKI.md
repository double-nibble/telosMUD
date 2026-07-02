# List of ideas that I'm dumping while other things are happening so I don't forget

## Documentation

* Create the Wiki
* Getting Started
    * Basic summary/history
        * TTRPGs
        * MUDs
        * Later MMOs
    * How to run it
    * How to set up auth
    * How to install a pack
    * Included packs, how to remove them
* Documentation
    * Builder reference
        * Core supported Commands
        * How Packs work
        * Any other builder-level info
        * Pack reference
            * Itemized reference for packs
            * Every time of entity
            * How to write scripts
            * Any hooks for scripts
            * Everything basically
    * Sysadmin reference
        * Deployment
        * Walkthrough of IaC
        * Baremetal/VMs versus EKS/K8S?
        * How to run things at scale
        * Metrics
        * Logging
    * Developer reference
        * Itemized list of concepts
        * How every single component of the system works
            * Don't focus too much on documenting the code itself (this is already done), focus on algorithms and API
            * Go through every file in `docs` and include its info
            * Message flow diagrams
            * Sequence diagrams
            * Etc
    * GMCP reference
        * Everything we include in the base and how to extend it
        * Mudlet samples?
            * UI for HP bar
            * Automapper
            * Tab-complete via `addCmdLineSuggestion(gmcp.Char.Items.name loop)` on new `GMCP` event handler

## Cleanups

* Last step is scrubbing
* Remove `docs` directory
    * Each file is one of two things
        1. Planning/roadmaps --- Unimportant, delete them, make sure every task is done and any relevant decision is captured
        2. Brainstorms/ideas --- Make sure it's documented in the Wiki
* Every comment across every file in the code base needs to be scrubbed
    * No reference to "Kurt" or "User" or anything like that
    * The docs should explain code, there is no reason to explain thought process
    * Use a passive voice, no first person
        * Example: "During testing we found that this break if..."
        * Fix: "During testing, it was found that this breaks if..."
    * Be direct about what is happening, no need to tell a story
    * Don't mention anything about any planning
        * Example: "Adds logging (Phase 12)" --- no need for that, don't mention any planning anywhere
    * Don't mention any docs in code comments



