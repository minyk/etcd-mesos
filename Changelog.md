## 0.1.3.2
 
- Move from ancient go 1.4 to go 1.8.1
- Replace dependency management with standard glide
- Remove horrific use of git modules
- Make it easier to move to etcd 1.3.xx
- Build scheduler with no shared libs.
- Strip binaries 
- shrink final docker image to 56M from
- No golang required by user - fully dockerized build
- Dev shell with `make run-dev`.
   - Includes bash history  

